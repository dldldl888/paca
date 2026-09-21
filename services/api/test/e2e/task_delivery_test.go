package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
)

// Preserve the real socket's deadline and flush support. Pause before any
// headers/bytes leave the server, and independently before its actual flush.
type heldTaskJSON struct {
	http.ResponseWriter
	headerEntered                             chan int
	headerRelease, flushEntered, flushRelease chan struct{}
	wroteHeader                               bool
	flushOnce                                 sync.Once
}

func (w *heldTaskJSON) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *heldTaskJSON) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.headerEntered <- status
	<-w.headerRelease
	w.ResponseWriter.WriteHeader(status)
}
func (w *heldTaskJSON) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *heldTaskJSON) FlushError() error {
	w.flushOnce.Do(func() { close(w.flushEntered); <-w.flushRelease })
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Removing a read lease, releasing it before Flush, or forgetting the gate in
// the real route assembly must expose a committed reassignment before delivery.
func TestTaskJSONDeliveryFlushesBeforeReassignmentCommit(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "task-delivery-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "task-delivery-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, mc, mt))
	manager, err := env.userRepo.FindByUsername(env.ctx, "task-delivery-manager")
	if err != nil {
		t.Fatal(err)
	}
	mctx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "task-delivery-reader", "password1", "Task Delivery Reader")
	reader, err := env.userRepo.FindByUsername(env.ctx, "task-delivery-reader")
	if err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	if _, err = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, reader.ID); err != nil {
		t.Fatal(err)
	}
	rc, rt := taskMemberLogin(t, env, "task-delivery-reader", "password1")

	for _, name := range []string{"detail", "list", "by_number", "assigned", "activities", "links"} {
		t.Run(name, func(t *testing.T) {
			sentinel := "task-delivery-secret-" + name
			points := 7
			task, err := env.taskSvc.CreateTask(mctx, taskdom.CreateTaskInput{ProjectID: pid, Title: sentinel, StoryPoints: &points, AssigneeIDs: []uuid.UUID{member}})
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/projects/" + pid.String() + "/tasks/" + task.ID.String()
			wantAfter := http.StatusNotFound
			switch name {
			case "list":
				path = "/api/v1/projects/" + pid.String() + "/tasks?sum_field=story_points"
				wantAfter = http.StatusOK
			case "by_number":
				path = fmt.Sprintf("/api/v1/projects/%s/tasks/by-number/%d", pid, task.TaskNumber)
			case "assigned":
				path = "/api/v1/users/me/tasks"
				wantAfter = http.StatusOK
			case "activities":
				_, err = env.activitySvc.AddComment(mctx, taskdom.AddCommentInput{ProjectID: pid, TaskID: task.ID, ActorID: manager.ID, Content: json.RawMessage(fmt.Sprintf(`{"text":%q}`, sentinel))})
				if err != nil {
					t.Fatal(err)
				}
				path += "/activities"
			case "links":
				target, e := env.taskSvc.CreateTask(mctx, taskdom.CreateTaskInput{ProjectID: pid, Title: sentinel + "-linked", AssigneeIDs: []uuid.UUID{member}})
				if e != nil {
					t.Fatal(e)
				}
				_, err = env.taskSvc.CreateTaskLink(mctx, taskdom.CreateTaskLinkInput{ProjectID: pid, SourceTaskID: task.ID, TargetTaskID: target.ID, LinkType: taskdom.LinkType("blocks")})
				if err != nil {
					t.Fatal(err)
				}
				path += "/links"
			}
			waitFileGate(t, env)
			before, err := coordinator.State(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			headerEntered := make(chan int, 1)
			headerRelease, flushEntered, flushRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var headerOnce, flushOnce sync.Once
			releaseHeader := func() { headerOnce.Do(func() { close(headerRelease) }) }
			releaseFlush := func() { flushOnce.Do(func() { close(flushRelease) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				env.handler.ServeHTTP(&heldTaskJSON{ResponseWriter: w, headerEntered: headerEntered, headerRelease: headerRelease, flushEntered: flushEntered, flushRelease: flushRelease}, r)
			}))
			defer func() { releaseHeader(); releaseFlush(); server.Close() }()
			type response struct {
				status int
				body   string
				err    error
			}
			delivered := make(chan response, 1)
			req := mustRequest(env.ctx, t, "GET", server.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+rt)
			go func() {
				resp, e := rc.Do(req)
				if e != nil {
					delivered <- response{err: e}
					return
				}
				body, e := io.ReadAll(resp.Body)
				resp.Body.Close()
				delivered <- response{resp.StatusCode, string(body), e}
			}()
			select {
			case status := <-headerEntered:
				if status != http.StatusOK {
					t.Fatalf("initial authorized read status=%d, want 200", status)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("read never reached first WriteHeader")
			}
			committed := make(chan error, 1)
			go func() {
				empty := []uuid.UUID{}
				_, e := env.taskSvc.UpdateTask(mctx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &empty})
				committed <- e
			}()
			deadline := time.After(3 * time.Second)
			for !env.fileGate.State().Blocked {
				select {
				case e := <-committed:
					releaseHeader()
					releaseFlush()
					old := <-delivered
					t.Fatalf("reassignment committed before first WriteHeader: err=%v old_status=%d old_sentinel=%t response_err=%v", e, old.status, strings.Contains(old.body, sentinel), old.err)
				case <-deadline:
					t.Fatal("reassignment never entered real coordinator barrier")
				case <-time.After(time.Millisecond):
				}
			}
			// Give the coordinator time to finish if this handler has no lease.
			select {
			case e := <-committed:
				waitFileGate(t, env)
				after, stateErr := coordinator.State(env.ctx)
				check := mustRequest(env.ctx, t, "GET", env.base+path, nil)
				check.Header.Set("Authorization", "Bearer "+rt)
				fresh := mustDo(t, rc, check)
				freshBody, readErr := io.ReadAll(fresh.Body)
				fresh.Body.Close()
				releaseHeader()
				releaseFlush()
				old := <-delivered
				t.Fatalf("committed before first WriteHeader: mutation=%v epoch=%s->%s state_err=%v fresh_status=%d fresh_sentinel=%t fresh_err=%v old_status=%d old_sentinel=%t old_err=%v", e, before.Epoch, after.Epoch, stateErr, fresh.StatusCode, strings.Contains(string(freshBody), sentinel), readErr, old.status, strings.Contains(old.body, sentinel), old.err)
			case <-time.After(100 * time.Millisecond):
			}
			assertWaiting := func(stage string) {
				t.Helper()
				select {
				case e := <-committed:
					t.Fatalf("reassignment committed before %s: %v", stage, e)
				case <-time.After(100 * time.Millisecond):
				}
				var assigned int
				if e := env.db.Get(&assigned, `SELECT count(*) FROM task_assignees WHERE task_id=$1 AND member_id=$2`, task.ID, member); e != nil || assigned != 1 {
					t.Fatalf("assignment changed before %s: count=%d err=%v", stage, assigned, e)
				}
			}
			assertWaiting("WriteHeader")
			releaseHeader()
			select {
			case <-flushEntered:
			case e := <-committed:
				t.Fatalf("reassignment committed before explicit HTTP flush: %v", e)
			case <-time.After(3 * time.Second):
				t.Fatal("response never entered actual flush")
			}
			assertWaiting("Flush")
			releaseFlush()
			select {
			case old := <-delivered:
				if old.err != nil || old.status != http.StatusOK || !strings.Contains(old.body, sentinel) {
					t.Fatalf("legal response not delivered: status=%d sentinel=%t err=%v", old.status, strings.Contains(old.body, sentinel), old.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("legal response did not complete")
			}
			select {
			case e := <-committed:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("reassignment did not commit after flush")
			}
			waitFileGate(t, env)
			after, err := coordinator.State(env.ctx)
			if err != nil || after.Epoch == before.Epoch || after.Blocked {
				t.Fatalf("coordinator did not advance and reopen: before=%+v after=%+v err=%v", before, after, err)
			}
			req = mustRequest(env.ctx, t, "GET", env.base+path, nil)
			req.Header.Set("Authorization", "Bearer "+rt)
			resp := mustDo(t, rc, req)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != wantAfter || strings.Contains(string(body), sentinel) {
				t.Fatalf("post-reassignment read status=%d want=%d sentinel=%t err=%v", resp.StatusCode, wantAfter, strings.Contains(string(body), sentinel), err)
			}
			if name == "list" {
				var data struct {
					Data struct {
						TotalCount int     `json:"total_count"`
						FieldSum   float64 `json:"field_sum"`
					} `json:"data"`
				}
				if err = json.Unmarshal(body, &data); err != nil || data.Data.TotalCount != 0 || data.Data.FieldSum != 0 {
					t.Fatalf("revoked aggregates: %+v err=%v", data.Data, err)
				}
			}
			t.Logf("200 sentinel delivered before supported reassignment; epoch %s -> %s; same-token read=%d", before.Epoch, after.Epoch, resp.StatusCode)
		})
	}

	// A blanket lease around TaskHandler would deadlock authorityMutation.
	t.Run("write_and_progress_controls", func(t *testing.T) {
		task, err := env.taskSvc.CreateTask(mctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "write control", AssigneeIDs: []uuid.UUID{member}})
		if err != nil {
			t.Fatal(err)
		}
		var status uuid.UUID
		if err = env.db.Get(&status, `SELECT id FROM task_statuses WHERE project_id=$1 AND is_default=true`, pid); err != nil {
			t.Fatal(err)
		}
		for _, control := range []struct {
			token string
			body  any
		}{{rt, map[string]any{"status_id": status}}, {mt, map[string]any{"title": "manager update"}}, {mt, map[string]any{"assignee_ids": []uuid.UUID{}}}} {
			ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
			req := mustRequest(ctx, t, "PATCH", env.base+"/api/v1/projects/"+pid.String()+"/tasks/"+task.ID.String(), jsonBody(t, control.body))
			req.Header.Set("Authorization", "Bearer "+control.token)
			req.Header.Set("Content-Type", "application/json")
			resp := mustDo(t, mc, req)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			assertStatus(t, resp, http.StatusOK)
			waitFileGate(t, env)
		}
	})
}

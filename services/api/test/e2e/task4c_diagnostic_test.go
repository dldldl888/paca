package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	notificationdom "github.com/Paca-AI/api/internal/domain/notification"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	notificationsvc "github.com/Paca-AI/api/internal/service/notification"
	"github.com/google/uuid"
)

func TestTask4cPostWriteAndNotificationDelivery(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "task4c-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "task4c-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, mc, mt))
	manager, err := env.userRepo.FindByUsername(env.ctx, "task4c-manager")
	if err != nil {
		t.Fatal(err)
	}
	mctx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "task4c-reader", "password1", "Task4c Reader")
	reader, err := env.userRepo.FindByUsername(env.ctx, "task4c-reader")
	if err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	if _, err = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, reader.ID); err != nil {
		t.Fatal(err)
	}
	rc, rt := taskMemberLogin(t, env, "task4c-reader", "password1")
	var managerMember uuid.UUID
	if err = env.db.Get(&managerMember, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, manager.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		status int
		body   string
		err    error
	}
	read := func(client *http.Client, req *http.Request) result {
		resp, err := client.Do(req)
		if err != nil {
			return result{err: err}
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return result{resp.StatusCode, string(body), err}
	}
	request := func(token, method, url string, body any) *http.Request {
		req := mustRequest(env.ctx, t, method, url, jsonBody(t, body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req
	}

	for _, name := range []string{"progress_response", "notification_list"} {
		t.Run(name, func(t *testing.T) {
			sentinel := "TASK4C_SYNTHETIC_SECRET_" + name
			task, err := env.taskSvc.CreateTask(mctx, taskdom.CreateTaskInput{ProjectID: pid, Title: sentinel, AssigneeIDs: []uuid.UUID{member}})
			if err != nil {
				t.Fatal(err)
			}
			taskPath := "/api/v1/projects/" + pid.String() + "/tasks/" + task.ID.String()
			path, method := taskPath, "PATCH"
			var body any
			if name == "progress_response" {
				if task.StatusID == nil {
					t.Fatal("fixture needs a default status")
				}
				var other uuid.UUID
				if err = env.db.Get(&other, `SELECT id FROM task_statuses WHERE project_id=$1 AND id<>$2 ORDER BY position LIMIT 1`, pid, *task.StatusID); err != nil {
					t.Fatal(err)
				}
				control := read(rc, request(rt, "PATCH", env.base+path, map[string]any{"status_id": other}))
				if control.err != nil || control.status != 200 || !strings.Contains(control.body, sentinel) {
					t.Fatalf("legal progress control: status=%d sentinel=%t err=%v", control.status, strings.Contains(control.body, sentinel), control.err)
				}
				body = map[string]any{"status_id": task.StatusID}
			} else {
				repo := pg.NewNotificationRepository(env.db)
				notify := notificationsvc.New(repo, env.projectRepo, nil).WithCurrentDelivery(env.fileGate, env.taskSvc)
				if err = notify.NotifyAssigned(mctx, notificationdom.NotifyAssignedInput{TaskID: task.ID, ProjectID: pid, NewAssigneeMemberID: member, ActorUserID: manager.ID, SourceEventID: "task4c-synthetic-assignment"}); err != nil {
					t.Fatal(err)
				}
				path, method = "/api/v1/users/me/notifications", "GET"
				control := read(rc, request(rt, method, env.base+path, nil))
				if control.err != nil || control.status != 200 || !strings.Contains(control.body, sentinel) || !strings.Contains(control.body, task.ID.String()) {
					t.Fatalf("legal notification control: status=%d sentinel=%t task_id=%t err=%v", control.status, strings.Contains(control.body, sentinel), strings.Contains(control.body, task.ID.String()), control.err)
				}
			}
			waitFileGate(t, env)
			before, err := coordinator.State(env.ctx)
			if err != nil || before.Blocked {
				t.Fatalf("initial coordinator: %+v %v", before, err)
			}
			headerEntered := make(chan int, 1)
			headerRelease, flushEntered, flushRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var headerOnce, flushOnce sync.Once
			release := func() { headerOnce.Do(func() { close(headerRelease) }); flushOnce.Do(func() { close(flushRelease) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				env.handler.ServeHTTP(&heldTaskJSON{ResponseWriter: w, headerEntered: headerEntered, headerRelease: headerRelease, flushEntered: flushEntered, flushRelease: flushRelease}, r)
			}))
			defer func() { release(); server.Close() }()
			held := request(rt, method, server.URL+path, body)
			delivered := make(chan result, 1)
			go func() { delivered <- read(rc, held) }()
			select {
			case status := <-headerEntered:
				if status != 200 {
					t.Fatalf("first WriteHeader must be authorized 200, got %d", status)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("did not reach first WriteHeader")
			}
			if name == "progress_response" {
				var persisted uuid.UUID
				if err = env.db.Get(&persisted, `SELECT status_id FROM tasks WHERE id=$1`, task.ID); err != nil || persisted != *task.StatusID {
					t.Fatalf("business write was not committed before response pause: status=%s err=%v", persisted, err)
				}
			}
			mutationReq := request(mt, "PATCH", env.base+taskPath, map[string]any{"assignee_ids": []uuid.UUID{managerMember}})
			committed := make(chan result, 1)
			go func() { committed <- read(mc, mutationReq) }()
			committedBeforeDelivery := false
			var mutation result
			select {
			case mutation = <-committed:
				committedBeforeDelivery = true
			case <-time.After(500 * time.Millisecond):
				if !env.fileGate.State().Blocked {
					t.Fatal("mutation neither committed nor entered real barrier")
				}
				release()
				select {
				case mutation = <-committed:
				case <-time.After(3 * time.Second):
					t.Fatal("mutation did not finish after legal response release")
				}
			}
			if mutation.err != nil || mutation.status != 200 {
				t.Fatalf("supported manager reassignment failed: status=%d err=%v", mutation.status, mutation.err)
			}
			waitFileGate(t, env)
			after, err := coordinator.State(env.ctx)
			if err != nil || after.Blocked || after.Epoch == before.Epoch {
				t.Fatalf("revocation not committed/reopened: before=%+v after=%+v err=%v", before, after, err)
			}
			freshTask := read(rc, request(rt, "GET", env.base+taskPath, nil))
			if freshTask.err != nil || freshTask.status != 404 || strings.Contains(freshTask.body, sentinel) {
				t.Fatalf("fresh task read must deny: status=%d sentinel=%t err=%v", freshTask.status, strings.Contains(freshTask.body, sentinel), freshTask.err)
			}
			freshStatus := freshTask.status
			if name == "notification_list" {
				fresh := read(rc, request(rt, "GET", env.base+path, nil))
				if fresh.err != nil || fresh.status != 200 || strings.Contains(fresh.body, sentinel) || strings.Contains(fresh.body, task.ID.String()) {
					t.Fatalf("fresh notification leaked task: status=%d sentinel=%t task_id=%t err=%v", fresh.status, strings.Contains(fresh.body, sentinel), strings.Contains(fresh.body, task.ID.String()), fresh.err)
				}
				var envelope struct {
					Data struct {
						Items []struct {
							TaskID     *uuid.UUID `json:"task_id"`
							TaskTitle  string     `json:"task_title"`
							TaskNumber int        `json:"task_number"`
						}
						UnreadCount int `json:"unread_count"`
					}
				}
				if err = json.Unmarshal([]byte(fresh.body), &envelope); err != nil || len(envelope.Data.Items) != 1 || envelope.Data.UnreadCount != 1 {
					t.Fatalf("personal notification/unread must survive: rows=%d unread=%d err=%v", len(envelope.Data.Items), envelope.Data.UnreadCount, err)
				}
				item := envelope.Data.Items[0]
				if item.TaskID != nil || item.TaskTitle != "" || item.TaskNumber != 0 {
					t.Fatal("fresh task projection not masked")
				}
				freshStatus = fresh.status
			}
			release()
			var old result
			select {
			case old = <-delivered:
			case <-time.After(3 * time.Second):
				t.Fatal("held response did not finish")
			}
			if old.err != nil || old.status != 200 {
				t.Fatalf("held response is not a real successful response: status=%d err=%v", old.status, old.err)
			}
			sawFlush := false
			select {
			case <-flushEntered:
				sawFlush = true
			default:
			}
			leakedTitle, leakedID := strings.Contains(old.body, sentinel), strings.Contains(old.body, task.ID.String())
			t.Logf("method=%s pending_status=200 manager_patch=%d epoch=%s->%s gate_open=%t committed_before_first_header=%t fresh_task=%d fresh_surface=%d old_status=%d old_sentinel=%t old_task_id=%t explicit_flush_observed=%t", method, mutation.status, before.Epoch, after.Epoch, !after.Blocked, committedBeforeDelivery, freshTask.status, freshStatus, old.status, leakedTitle, leakedID, sawFlush)
			if committedBeforeDelivery && (leakedTitle || leakedID) {
				t.Error("REVOKED_BODY_CROSSED_COMMITTED_REASSIGNMENT")
			}
			if !committedBeforeDelivery && (!leakedTitle || !leakedID) {
				t.Error("lease branch must deliver original legal body")
			}
		})
	}
}

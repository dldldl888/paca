package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	notificationdom "github.com/Paca-AI/api/internal/domain/notification"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	notificationsvc "github.com/Paca-AI/api/internal/service/notification"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestAuthorizationDeliveryRealHTTPAndSocketReconnect(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "delivery-manager", "password1")
	client, token := taskMemberLogin(t, env, "delivery-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, _ := env.userRepo.FindByUsername(env.ctx, "delivery-manager")
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	var users, members [2]uuid.UUID
	var tokens [2]string
	for i, name := range []string{"delivery-a", "delivery-b"} {
		seedUser(t, env, name, "password1", name)
		u, e := env.userRepo.FindByUsername(env.ctx, name)
		if e != nil {
			t.Fatal(e)
		}
		users[i] = u.ID
		members[i] = uuid.New()
		if _, e = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, members[i], pid, u.ID); e != nil {
			t.Fatal(e)
		}
		_, tokens[i] = taskMemberLogin(t, env, name, "password1")
	}
	task, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "lawful-before-close", AssigneeIDs: []uuid.UUID{members[0]}})
	if err != nil {
		t.Fatal(err)
	}
	instance := uuid.New()
	gate := env.fileGate
	d := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, docsvc.New(pg.NewDocumentRepository(env.db), env.projectRepo), nil, env.automationSvc).WithDeliveryGate(gate)
	d.Reconcile = env.agentSvc.ReconcilePrivateRuns
	stop, err := d.StartParticipant(env.ctx, gate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := stop(ctx); e != nil {
			t.Error(e)
		}
	}()
	if err = d.Mutate(env.ctx, nil); err != nil {
		t.Fatal(err)
	}
	// OPEN is asynchronous; observe the API participant before creating notifications.
	for gate.State().Blocked {
		select {
		case <-env.ctx.Done():
			t.Fatal(env.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	d.Install()
	// Known typed references must be checked even when publication precedes row persistence.
	hidden, e := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "hidden reference", AssigneeIDs: []uuid.UUID{members[1]}})
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name, kind, body string
		wantA            bool
	}{
		{"hidden_related_task", "task.link.added", fmt.Sprintf(`{"target_task_id":%q}`, hidden.ID), false},
		{"visible_related_task", "task.link.added", fmt.Sprintf(`{"target_task_id":%q}`, task.ID), true},
		{"missing_metadata", "", "", false},
		{"malformed_metadata", "task.link.added", `null`, false},
	} {
		scope := agentsvc.RealtimeScope{EventType: "task.link.added", ProjectID: pid.String(), TaskID: task.ID.String(), ActivityType: tc.kind, Content: json.RawMessage(tc.body)}
		recipients, e := env.agentSvc.CurrentRecipients(env.ctx, scope)
		if e != nil {
			t.Fatal(tc.name, e)
		}
		found := false
		for _, id := range recipients.UserIDs {
			if id == users[0].String() {
				found = true
			}
		}
		if found != tc.wantA {
			t.Fatalf("%s recipient A=%v", tc.name, found)
		}
	}
	notifications := pg.NewNotificationRepository(env.db)
	notify := notificationsvc.New(notifications, env.projectRepo, nil).WithCurrentDelivery(gate, env.taskSvc)
	input := notificationdom.NotifyAssignedInput{ProjectID: pid, TaskID: task.ID, ActorUserID: manager.ID, NewAssigneeMemberID: members[0]}
	if err = notify.NotifyAssigned(env.ctx, input); err != nil {
		t.Fatal(err)
	}
	initial, _, err := notifications.ListForUser(env.ctx, users[0], 20, nil)
	if err != nil || len(initial) != 1 {
		t.Fatalf("lawful notification: %v %v", initial, err)
	}
	var hold atomic.Bool
	var computedOnce, releaseOnce sync.Once
	computed := make(chan struct{})
	release := make(chan struct{})
	defer releaseOnce.Do(func() { close(release) })
	var authCount atomic.Int32
	var recipientRetries atomic.Int32
	key := "fixture-internal"
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any, e error) {
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"api": env.base, "managerToken": token, "redis": sharedRedisURL, "key": key, "instance": instance, "project": pid, "task": task.ID, "userA": users[0], "memberB": members[1], "tokenA": tokens[0], "tokenB": tokens[1], "notification": initial[0].ID})
	})
	mux.HandleFunc("/api/v1/users/me/global-permissions", func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", env.base+r.URL.Path, nil)
		req.Header = r.Header.Clone()
		resp, e := env.client.Do(req)
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		defer resp.Body.Close()
		authCount.Add(1)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	mux.HandleFunc("/api/internal/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != key {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/internal/realtime/recipients":
			if recipientRetries.Add(-1) >= 0 {
				http.Error(w, "authorization transition pending", http.StatusConflict)
				return
			}
			var scope agentsvc.RealtimeScope
			if e := json.NewDecoder(r.Body).Decode(&scope); e != nil {
				http.Error(w, "bad scope", 400)
				return
			}
			result, e := env.agentSvc.CurrentRecipients(r.Context(), scope)
			if e != nil {
				reply(w, nil, e)
				return
			}
			// Hold already-computed bytes after the production authorization lease ended.
			data, _ := json.Marshal(map[string]any{"success": true, "data": result})
			if hold.Swap(false) {
				computedOnce.Do(func() { close(computed) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		case "/api/internal/authorization-delivery/state":
			state, e := d.State(r.Context())
			reply(w, state, e)
		case "/api/internal/authorization-delivery/participants":
			var in struct {
				BootID string `json:"boot_id"`
				Kind   string `json:"kind"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			state, e := d.Enroll(r.Context(), in.BootID, in.Kind)
			reply(w, state, e)
		default:
			if r.Method == "DELETE" {
				e := d.Unenroll(r.Context(), filepath.Base(r.URL.Path))
				reply(w, map[string]bool{"removed": e == nil}, e)
				return
			}
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/retry-recipients", func(w http.ResponseWriter, r *http.Request) { recipientRetries.Store(2) })
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) { hold.Store(true) })
	mux.HandleFunc("/computed", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-computed:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) { releaseOnce.Do(func() { close(release) }) })
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		assignees := []uuid.UUID{members[1]}
		_, e := env.taskSvc.UpdateTask(managerCtx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &assignees})
		reply(w, true, e)
	})
	mux.HandleFunc("/restore", func(w http.ResponseWriter, r *http.Request) {
		assignees := []uuid.UUID{members[0]}
		_, e := env.taskSvc.UpdateTask(managerCtx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &assignees})
		reply(w, true, e)
	})
	mux.HandleFunc("/truth", func(w http.ResponseWriter, r *http.Request) {
		var ids []string
		e := env.db.Select(&ids, `SELECT member_id::text FROM task_assignees WHERE task_id=$1`, task.ID)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assignees": ids})
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tokens[0] {
			http.Error(w, "unauthorized", 401)
			return
		}
		if _, e := env.db.Exec(`UPDATE tasks SET title='FRESH_HIDDEN_TITLE' WHERE id=$1`, task.ID); e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		if e := notify.NotifyAssigned(r.Context(), input); e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		rows, _, e := notifications.ListForUser(r.Context(), users[0], 20, nil)
		if e != nil || len(rows) != 1 {
			http.Error(w, fmt.Sprintf("delayed notification rows=%d err=%v", len(rows), e), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("/auth-count", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": authCount.Load()})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := filepath.Abs("../../../realtime")
	if err != nil {
		t.Fatal(err)
	}
	req := testcontainers.ContainerRequest{Image: "oven/bun:1-alpine", WorkingDir: "/app", Env: map[string]string{"PACA_DELIVERY_FIXTURE": server.URL}, Cmd: []string{"sh", "-c", "bun install --frozen-lockfile && bun test src/server.test.ts"}, Files: []testcontainers.ContainerFile{{HostFilePath: filepath.Join(realtime, "src"), ContainerFilePath: "/app/src", FileMode: 0755}, {HostFilePath: filepath.Join(realtime, "package.json"), ContainerFilePath: "/app/package.json", FileMode: 0644}, {HostFilePath: filepath.Join(realtime, "bun.lock"), ContainerFilePath: "/app/bun.lock", FileMode: 0644}}, HostConfigModifier: func(cfg *container.HostConfig) { cfg.NetworkMode = container.NetworkMode("container:" + hostname) }, WaitingFor: wait.ForExit().WithExitTimeout(90 * time.Second)}
	bun, err := testcontainers.GenericContainer(env.ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if bun != nil {
		defer bun.Terminate(context.Background())
		logs, e := bun.Logs(env.ctx)
		if e == nil {
			data, _ := io.ReadAll(logs)
			logs.Close()
			t.Log(string(data))
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	state, err := bun.State(env.ctx)
	if err != nil || state.ExitCode != 0 {
		t.Fatalf("real realtime acceptance exit: %+v %v", state, err)
	}
}

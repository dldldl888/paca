package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	pgRepo "github.com/Paca-AI/api/internal/repository/postgres"
	tasksvc "github.com/Paca-AI/api/internal/service/task"
	"github.com/Paca-AI/api/migrations"
	"github.com/google/uuid"
)

// TestUS001CreatorMigration upgrades an actual pre-creator schema containing a
// reporter/assignee task. The migration must leave its creator unknown.
func TestUS001CreatorMigration(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "creator-migration", "password1")
	client, token := taskMemberLogin(t, env, "creator-migration", "password1")
	projectID := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	var memberID uuid.UUID
	if err := env.db.Get(&memberID, `SELECT id FROM project_members WHERE project_id=$1 LIMIT 1`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`ALTER TABLE tasks DROP COLUMN creator_user_id; DELETE FROM schema_migrations WHERE filename='000054_add_task_creator.sql'`); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO tasks(id,project_id,task_number,title,reporter_id) VALUES($1,$2,1,'old task',$3)`, id, projectID, memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, id, memberID); err != nil {
		t.Fatal(err)
	}
	if err := database.RunMigrationsFS(env.db.DB, migrations.FS); err != nil {
		t.Fatal(err)
	}
	task, err := env.taskRepo.FindTaskByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.CreatorUserID != nil || task.ReporterID == nil || *task.ReporterID != memberID || len(task.AssigneeIDs) != 1 || task.AssigneeIDs[0] != memberID {
		t.Fatalf("migration changed attribution: %+v", task)
	}
}

// A single real database fixture exercises the public service and HTTP task
// boundaries, including current role/member changes without refreshing tokens.
func TestUS002To004TaskIsolation(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "manager", "password1")
	managerClient, managerToken := taskMemberLogin(t, env, "manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, managerClient, managerToken))
	manager, err := env.userRepo.FindByUsername(env.ctx, "manager")
	if err != nil {
		t.Fatal(err)
	}
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	var members [2]uuid.UUID
	var users [2]uuid.UUID
	var clients [2]*http.Client
	var tokens [2]string
	var contexts [2]context.Context
	for i, name := range []string{"employee-a", "employee-b"} {
		seedUser(t, env, name, "password1", name)
		user, err := env.userRepo.FindByUsername(env.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		users[i] = user.ID
		members[i] = uuid.New()
		contexts[i] = authz.WithActorID(env.ctx, user.ID)
		if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, members[i], pid, user.ID); err != nil {
			t.Fatal(err)
		}
		clients[i], tokens[i] = taskMemberLogin(t, env, name, "password1")
	}
	tasks := make([]*taskdom.Task, 0, 4)
	for i := 0; i < 4; i++ {
		points := i + 1
		task, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: fmt.Sprintf("private-%d", i), AssigneeIDs: []uuid.UUID{members[i%2]}, StoryPoints: &points})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	request := func(t *testing.T, index int, method, path string, body map[string]any, status int) map[string]any {
		t.Helper()
		req := mustRequest(env.ctx, t, method, env.base+path, nil)
		if body != nil {
			req = mustRequest(env.ctx, t, method, env.base+path, jsonBody(t, body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tokens[index])
		resp := mustDo(t, clients[index], req)
		defer resp.Body.Close()
		assertStatus(t, resp, status)
		if status != http.StatusOK && status != http.StatusCreated {
			return nil
		}
		var result envelope
		decodeJSON(t, resp, &result)
		return assertDataMap(t, result)
	}
	base := "/api/v1/projects/" + pid.String() + "/tasks"
	t.Run("authenticated_immutable_creator", func(t *testing.T) {
		for _, task := range tasks {
			if task.CreatorUserID == nil || *task.CreatorUserID != manager.ID {
				t.Fatal("creator not authenticated manager")
			}
		}
		reporter := &members[1]
		updated, err := env.taskSvc.UpdateTask(managerCtx, pid, tasks[0].ID, taskdom.UpdateTaskInput{ReporterID: &reporter})
		if err != nil {
			t.Fatal(err)
		}
		if updated.CreatorUserID == nil || *updated.CreatorUserID != manager.ID {
			t.Fatal("reporter update changed creator")
		}
		if _, err := env.taskSvc.GetTask(contexts[1], pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("reporter granted access: %v", err)
		}
	})
	t.Run("bidirectional_id_number_read", func(t *testing.T) {
		for i := range 2 {
			for j, task := range tasks {
				want := http.StatusNotFound
				if j%2 == i {
					want = http.StatusOK
				}
				request(t, i, http.MethodGet, base+"/"+task.ID.String(), nil, want)
				request(t, i, http.MethodGet, fmt.Sprintf("%s/by-number/%d", base, task.TaskNumber), nil, want)
			}
		}
		if _, err := env.taskSvc.GetTask(context.Background(), pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("missing identity succeeded")
		}
		if _, err := env.taskSvc.GetTask(authz.WithAgentID(managerCtx, uuid.New()), pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("bot impersonated human")
		}
	})
	t.Run("pagination_search_count_sum_home_assigned", func(t *testing.T) {
		for i := range 2 {
			seen := map[string]bool{}
			cursor := ""
			for page := 0; page < 3; page++ {
				query := "?page_size=1&sum_field=story_points&search=private"
				if cursor != "" {
					query += "&cursor=" + url.QueryEscape(cursor)
				}
				data := request(t, i, http.MethodGet, base+query, nil, http.StatusOK)
				if data["total_count"] != float64(2) || data["field_sum"] != float64(4+2*i) {
					t.Fatalf("leaked aggregate: %+v", data)
				}
				items := data["items"].([]any)
				if len(items) != 1 {
					t.Fatalf("pagination removed unauthorized rows after limit: %+v", data)
				}
				id := items[0].(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatal("duplicate page")
				}
				seen[id] = true
				next, _ := data["next_cursor"].(string)
				if next == "" {
					break
				}
				cursor = next
			}
			if len(seen) != 2 {
				t.Fatalf("got %d visible tasks", len(seen))
			}
			for j, task := range tasks {
				if seen[task.ID.String()] != (j%2 == i) {
					t.Fatal("wrong visible task")
				}
			}
			foreign := members[1-i].String()
			data := request(t, i, http.MethodGet, base+"?assignee_id="+foreign, nil, http.StatusOK)
			if data["total_count"] != float64(0) {
				t.Fatalf("client filter expanded authorization: %+v", data)
			}
			injected := taskdom.TaskFilter{Visibility: &taskdom.AccessScope{ProjectID: pid, ReadAll: true}}
			visibleCount, err := env.taskSvc.CountTasks(contexts[i], pid, injected)
			if err != nil || visibleCount != 2 {
				t.Fatalf("client scope expanded count: %d %v", visibleCount, err)
			}
			visibleSum, err := env.taskSvc.SumTaskField(contexts[i], pid, injected, "story_points")
			if err != nil || visibleSum != float64(4+2*i) {
				t.Fatalf("client scope expanded sum: %v %v", visibleSum, err)
			}
			count, err := env.taskSvc.CountOpenTasksByProjects(contexts[i], []uuid.UUID{pid, uuid.New()})
			if err != nil || count != 2 {
				t.Fatalf("workspace count=%d err=%v", count, err)
			}
			assigned, more, err := env.taskSvc.ListAssignedTasksInProjects(contexts[i], []uuid.UUID{pid}, 1, nil)
			if err != nil || len(assigned) != 1 || !more {
				t.Fatalf("assigned page: %v %v %v", assigned, more, err)
			}
			for _, task := range assigned {
				if task.AssigneeIDs[0] != members[i] {
					t.Fatal("assigned leaked")
				}
			}
		}
	})

	t.Run("stale_progress_snapshot_rechecks_current_access", func(t *testing.T) {
		stale, err := env.taskRepo.FindTaskByID(env.ctx, tasks[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		scope, err := authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)).ResolveTaskAccess(contexts[0], pid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, stale.ID, taskdom.UpdateTaskInput{Title: "manager concurrent title"}); err != nil {
			t.Fatal(err)
		}
		stale.UpdatedAt = time.Now()
		if err := env.taskRepo.UpdateTaskProgress(contexts[0], stale, scope); err != nil {
			t.Fatal(err)
		}
		if stale.Title != "manager concurrent title" {
			t.Fatal("status persistence restored stale title")
		}
		replacement := []uuid.UUID{members[1]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, stale.ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		if err := env.taskRepo.UpdateTaskProgress(contexts[0], stale, scope); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("stale assignment grant used: %v", err)
		}
		replacement = []uuid.UUID{members[0]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, stale.ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=permissions - 'tasks.write' WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
			t.Fatal(err)
		}
		if err := env.taskRepo.UpdateTaskProgress(contexts[0], stale, scope); !errors.Is(err, taskdom.ErrTaskForbidden) {
			t.Fatalf("stale write permission used: %v", err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=permissions || '{"tasks.write":true}'::jsonb WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=now() WHERE id=$1`, members[0]); err != nil {
			t.Fatal(err)
		}
		if err := env.taskRepo.UpdateTaskProgress(contexts[0], stale, scope); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("stale membership used: %v", err)
		}
		if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=NULL WHERE id=$1`, members[0]); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("manager_only_fields_and_atomic_refusal", func(t *testing.T) {
		var nextStatus uuid.UUID
		if err := env.db.Get(&nextStatus, `SELECT id FROM task_statuses WHERE project_id=$1 AND id != $2 LIMIT 1`, pid, tasks[0].StatusID); err != nil {
			t.Fatal(err)
		}
		request(t, 0, http.MethodPatch, base+"/"+tasks[0].ID.String(), map[string]any{"status_id": nextStatus.String()}, http.StatusOK)
		before, err := env.taskRepo.FindTaskByID(env.ctx, tasks[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		var activityBefore int
		if err := env.db.Get(&activityBefore, `SELECT COUNT(*) FROM task_activities WHERE task_id=$1`, before.ID); err != nil {
			t.Fatal(err)
		}
		due := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		for _, body := range []map[string]any{{"title": "stolen"}, {"description": []map[string]any{{"type": "paragraph", "content": []any{}}}}, {"assignee_ids": []string{members[1].String()}}, {"due_date": due}, {"status_id": tasks[0].StatusID.String(), "due_date": due}, {"reporter_id": members[0].String()}} {
			request(t, 0, http.MethodPatch, base+"/"+before.ID.String(), body, http.StatusForbidden)
		}
		request(t, 0, http.MethodPost, base, map[string]any{"title": "unauthorized"}, http.StatusForbidden)
		request(t, 0, http.MethodDelete, base+"/"+before.ID.String(), nil, http.StatusForbidden)
		after, err := env.taskRepo.FindTaskByID(env.ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Title != before.Title || after.DueDate != nil || *after.StatusID != *before.StatusID || after.UpdatedAt != before.UpdatedAt {
			t.Fatal("rejected mixed update mutated task")
		}
		var activityAfter, count int
		if err := env.db.Get(&activityAfter, `SELECT COUNT(*) FROM task_activities WHERE task_id=$1`, before.ID); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&count, `SELECT COUNT(*) FROM tasks WHERE project_id=$1`, pid); err != nil {
			t.Fatal(err)
		}
		if activityAfter != activityBefore || count != 4 {
			t.Fatal("rejection produced DB/activity effects")
		}
	})

	t.Run("current_global_permission_overrides_stale_jwt", func(t *testing.T) {
		foreignProject, foreignTask := uuid.New(), uuid.New()
		if _, err := env.db.Exec(`INSERT INTO projects(id,name) VALUES($1,'foreign-project')`, foreignProject); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`INSERT INTO tasks(id,project_id,task_number,title) VALUES($1,$2,1,'foreign-secret')`, foreignTask, foreignProject); err != nil {
			t.Fatal(err)
		}
		if _, err := env.taskSvc.GetTask(managerCtx, foreignProject, foreignTask); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("project manager crossed projects")
		}
		assignGlobalRolesByName(t, env, "manager", "SUPER_ADMIN")
		adminClient, adminToken := taskMemberLogin(t, env, "manager", "password1")
		if _, err := env.taskSvc.GetTask(managerCtx, foreignProject, foreignTask); err != nil {
			t.Fatalf("current global admin denied: %v", err)
		}
		assignGlobalRolesByName(t, env, "manager", "USER")
		req := mustRequest(env.ctx, t, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%s/tasks/%s", env.base, foreignProject, foreignTask), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp := mustDo(t, adminClient, req)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusForbidden)
	})
	t.Run("creator_and_live_revocation", func(t *testing.T) {
		// Give A management temporarily: its authenticated creator remains A after demotion/reassignment.
		var managerRole, memberRole uuid.UUID
		if err := env.db.Get(&managerRole, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Admin'`, pid); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&memberRole, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_members SET project_role_id=$1 WHERE id=$2`, managerRole, members[0]); err != nil {
			t.Fatal(err)
		}
		points := 9
		created, err := env.taskSvc.CreateTask(contexts[0], taskdom.CreateTaskInput{ProjectID: pid, Title: "created-by-a", AssigneeIDs: []uuid.UUID{members[1]}, StoryPoints: &points})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_members SET project_role_id=$1 WHERE id=$2`, memberRole, members[0]); err != nil {
			t.Fatal(err)
		}
		request(t, 0, http.MethodGet, base+"/"+created.ID.String(), nil, http.StatusNotFound)
		request(t, 0, http.MethodGet, fmt.Sprintf("%s/by-number/%d", base, created.TaskNumber), nil, http.StatusNotFound)
		data := request(t, 0, http.MethodGet, base+"?page_size=1&sum_field=story_points&search=created-by-a", nil, http.StatusOK)
		if data["total_count"] != float64(0) || data["field_sum"] != float64(0) || len(data["items"].([]any)) != 0 {
			t.Fatalf("creator-only task leaked through HTTP collection: %+v", data)
		}
		search := "created-by-a"
		scope, err := authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)).ResolveTaskAccess(contexts[0], pid)
		if err != nil {
			t.Fatal(err)
		}
		filter := taskdom.TaskFilter{Search: &search, Visibility: &scope}
		rows, more, err := env.taskRepo.ListTasks(env.ctx, pid, filter, 1, taskdom.TaskSort{})
		if err != nil || more || len(rows) != 0 {
			t.Fatalf("creator-only task leaked through SQL list: %+v %v %v", rows, more, err)
		}
		count, err := env.taskRepo.CountTasks(env.ctx, pid, filter)
		if err != nil || count != 0 {
			t.Fatalf("creator-only task leaked through SQL count: %d %v", count, err)
		}
		sum, err := env.taskRepo.SumTaskField(env.ctx, pid, filter, "story_points")
		if err != nil || sum != 0 {
			t.Fatalf("creator-only task leaked through SQL sum: %v %v", sum, err)
		}
		if _, err := env.taskSvc.UpdateTask(contexts[0], pid, created.ID, taskdom.UpdateTaskInput{}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("creator-only write: %v", err)
		}
		request(t, 1, http.MethodGet, base+"/"+created.ID.String(), nil, http.StatusOK)
		if _, err := env.taskSvc.GetTask(managerCtx, pid, created.ID); err != nil {
			t.Fatalf("project manager lost read: %v", err)
		}
		request(t, 0, http.MethodGet, base+"/"+tasks[0].ID.String(), nil, http.StatusOK)
		if _, err := env.taskSvc.GetTask(contexts[0], uuid.New(), created.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("creator crossed project: %v", err)
		}
		replacement := []uuid.UUID{members[1]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, tasks[0].ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		request(t, 0, http.MethodGet, base+"/"+tasks[0].ID.String(), nil, http.StatusNotFound)
		if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=now() WHERE id=$1`, members[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := env.taskSvc.GetTask(contexts[0], pid, created.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("removed creator can read")
		}
		perms, err := pgRepo.NewAuthzPermissionStore(env.db).ListProjectPermissions(env.ctx, users[0], pid)
		if err != nil || len(perms) != 0 {
			t.Fatalf("removed member retained route permissions: %v %v", perms, err)
		}
		if _, err := env.db.Exec(`UPDATE users SET deleted_at=now() WHERE id=$1`, users[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := env.taskSvc.GetTask(contexts[1], pid, created.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("deleted user can read")
		}
	})
}

// The read barrier controls the interleaving without a timing-based sleep.
type taskReadBarrierRepo struct {
	*pgRepo.TaskRepository
	read   chan struct{}
	resume chan struct{}
}

func (r *taskReadBarrierRepo) FindTaskByID(ctx context.Context, id uuid.UUID) (*taskdom.Task, error) {
	task, err := r.TaskRepository.FindTaskByID(ctx, id)
	if err == nil {
		close(r.read)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return task, err
}

func TestUS004ManagerPatchPreservesConcurrentAssignment(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "manager-race", "password1")
	client, token := taskMemberLogin(t, env, "manager-race", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, err := env.userRepo.FindByUsername(env.ctx, "manager-race")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(authz.WithActorID(env.ctx, manager.ID), 10*time.Second)
	defer cancel()
	users := make([]uuid.UUID, 2)
	members := make([]uuid.UUID, 2)
	for i, name := range []string{"race-a", "race-b"} {
		seedUser(t, env, name, "password1", name)
		user, err := env.userRepo.FindByUsername(env.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		users[i] = user.ID
		if err := env.db.Get(&members[i], `INSERT INTO project_members(project_id,user_id,project_role_id,member_type) SELECT $1,$2,id,'human' FROM project_roles WHERE project_id=$1 AND role_name='Editor' RETURNING id`, pid, user.ID); err != nil {
			t.Fatal(err)
		}
	}
	task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "before", AssigneeIDs: []uuid.UUID{members[0]}})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &taskReadBarrierRepo{TaskRepository: env.taskRepo, read: make(chan struct{}), resume: make(chan struct{})}
	svc := tasksvc.New(barrier, authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)))
	done := make(chan error, 1)
	go func() {
		_, err := svc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{Title: "manager title"})
		done <- err
	}()
	select {
	case <-barrier.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	replacement := []uuid.UUID{members[1]}
	if _, err := env.taskSvc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
		close(barrier.resume)
		t.Fatal(err)
	}
	close(barrier.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := env.taskRepo.FindTaskByID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Title != "manager title" || len(current.AssigneeIDs) != 1 || current.AssigneeIDs[0] != members[1] {
		t.Fatalf("title-only PATCH restored stale assignment: title=%q assignees=%v", current.Title, current.AssigneeIDs)
	}
	if _, err := env.taskSvc.GetTask(authz.WithActorID(env.ctx, users[0]), pid, task.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatalf("removed A regained access: %v", err)
	}
	if _, err := env.taskSvc.GetTask(authz.WithActorID(env.ctx, users[1]), pid, task.ID); err != nil {
		t.Fatalf("B lost current assignment: %v", err)
	}
}

func TestUS004ManagerPatchRechecksMergedParentType(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "parent-type-race", "password1")
	client, token := taskMemberLogin(t, env, "parent-type-race", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, err := env.userRepo.FindByUsername(env.ctx, "parent-type-race")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(authz.WithActorID(env.ctx, manager.ID), 10*time.Second)
	defer cancel()
	var epicID uuid.UUID
	if err := env.db.Get(&epicID, `SELECT id FROM task_types WHERE project_id=$1 AND is_system AND name='Epic'`, pid); err != nil {
		t.Fatal(err)
	}
	parent, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &taskReadBarrierRepo{TaskRepository: env.taskRepo, read: make(chan struct{}), resume: make(chan struct{})}
	svc := tasksvc.New(barrier, authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)))
	ep := &epicID
	done := make(chan error, 1)
	go func() {
		_, err := svc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{TaskTypeID: &ep})
		done <- err
	}()
	select {
	case <-barrier.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	parentID := &parent.ID
	if _, err := env.taskSvc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{ParentTaskID: &parentID}); err != nil {
		close(barrier.resume)
		t.Fatal(err)
	}
	close(barrier.resume)
	if err := <-done; !errors.Is(err, taskdom.ErrEpicCannotHaveParent) {
		t.Errorf("type-only PATCH did not reject the concurrently added parent: %v", err)
	}
	current, err := env.taskRepo.FindTaskByID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ParentTaskID == nil || *current.ParentTaskID != parent.ID || current.TaskTypeID == nil || *current.TaskTypeID != *task.TaskTypeID {
		t.Fatalf("rejected PATCH changed the valid committed pair: type=%v parent=%v", current.TaskTypeID, current.ParentTaskID)
	}
	// Explicitly clearing the parent and changing to Epic remains valid.
	var clearParent *uuid.UUID
	current, err = env.taskSvc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{TaskTypeID: &ep, ParentTaskID: &clearParent})
	if err != nil || current.ParentTaskID != nil || current.TaskTypeID == nil || *current.TaskTypeID != epicID {
		t.Fatalf("valid combined PATCH failed: task=%+v err=%v", current, err)
	}
}

type taskPolicyBarrier struct {
	taskdom.AccessPolicy
	checked chan struct{}
	resume  chan struct{}
}

func (p *taskPolicyBarrier) ResolveTaskAccess(ctx context.Context, pid uuid.UUID) (taskdom.AccessScope, error) {
	scope, err := p.AccessPolicy.ResolveTaskAccess(ctx, pid)
	if err == nil {
		close(p.checked)
		select {
		case <-p.resume:
		case <-ctx.Done():
			return taskdom.AccessScope{}, ctx.Err()
		}
	}
	return scope, err
}

func TestUS004ManagementMutationRechecksRevocation(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "manager-revoke", "password1")
	client, token := taskMemberLogin(t, env, "manager-revoke", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, err := env.userRepo.FindByUsername(env.ctx, "manager-revoke")
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithActorID(env.ctx, user.ID)
	var member, adminRole, editorRole uuid.UUID
	if err := env.db.Get(&member, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Get(&adminRole, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Admin'`, pid); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Get(&editorRole, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
		t.Fatal(err)
	}
	for _, revocation := range []string{"role", "membership", "user"} {
		for _, operation := range []string{"create", "update", "delete"} {
			t.Run(revocation+"_"+operation, func(t *testing.T) {
				task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "unchanged"})
				if err != nil {
					t.Fatal(err)
				}
				beforeCount, err := env.taskRepo.CountTasks(env.ctx, pid, taskdom.TaskFilter{})
				if err != nil {
					t.Fatal(err)
				}
				barrier := &taskPolicyBarrier{AccessPolicy: authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)), checked: make(chan struct{}), resume: make(chan struct{})}
				service := tasksvc.New(env.taskRepo, barrier)
				requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "create":
						_, err = service.CreateTask(requestCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "denied"})
					case "update":
						_, err = service.UpdateTask(requestCtx, pid, task.ID, taskdom.UpdateTaskInput{Title: "denied"})
					case "delete":
						err = service.DeleteTask(requestCtx, pid, task.ID)
					}
					done <- err
				}()
				select {
				case <-barrier.checked:
				case <-requestCtx.Done():
					t.Fatal(requestCtx.Err())
				}
				switch revocation {
				case "role":
					_, err = env.db.Exec(`UPDATE project_members SET project_role_id=$1 WHERE id=$2`, editorRole, member)
				case "membership":
					_, err = env.db.Exec(`UPDATE project_members SET deleted_at=now() WHERE id=$1`, member)
				case "user":
					_, err = env.db.Exec(`UPDATE users SET deleted_at=now() WHERE id=$1`, user.ID)
				}
				if err != nil {
					close(barrier.resume)
					t.Fatal(err)
				}
				close(barrier.resume)
				mutationErr := <-done
				// Restore the fixture before assertions so every subtest can run after a failure.
				if _, err := env.db.Exec(`UPDATE users SET deleted_at=NULL WHERE id=$1`, user.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=NULL,project_role_id=$1 WHERE id=$2`, adminRole, member); err != nil {
					t.Fatal(err)
				}
				if mutationErr == nil {
					t.Error("management mutation committed after revocation")
				}
				afterCount, err := env.taskRepo.CountTasks(env.ctx, pid, taskdom.TaskFilter{})
				if err != nil {
					t.Fatal(err)
				}
				current, err := env.taskRepo.FindTaskByID(env.ctx, task.ID)
				if err != nil || current.Title != "unchanged" || afterCount != beforeCount {
					t.Errorf("denied mutation changed storage: count %d -> %d, task=%+v, err=%v", beforeCount, afterCount, current, err)
				}
			})
		}
	}
}

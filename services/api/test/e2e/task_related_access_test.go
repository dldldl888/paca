package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Paca-AI/api/internal/events"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	sprintsvc "github.com/Paca-AI/api/internal/service/sprint"
	"github.com/google/uuid"
)

type afterTaskCheck struct {
	checker taskdom.AccessChecker
	hook    func()
}

func (c *afterTaskCheck) CheckTaskAccess(ctx context.Context, pid, id uuid.UUID, action taskdom.AccessAction) (*taskdom.Task, error) {
	task, err := c.checker.CheckTaskAccess(ctx, pid, id, action)
	if err == nil && c.hook != nil {
		hook := c.hook
		c.hook = nil
		hook()
	}
	return task, err
}

func TestUS005RelatedTaskBoundaries(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "related-manager", "password1")
	client, token := taskMemberLogin(t, env, "related-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, _ := env.userRepo.FindByUsername(env.ctx, "related-manager")
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	var member [2]uuid.UUID
	var userCtx [2]context.Context
	for i, name := range []string{"related-a", "related-b"} {
		seedUser(t, env, name, "password1", name)
		u, err := env.userRepo.FindByUsername(env.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		member[i] = uuid.New()
		userCtx[i] = authz.WithActorID(env.ctx, u.ID)
		if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member[i], pid, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	var task [3]*taskdom.Task
	for i := range task {
		var err error
		task[i], err = env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: fmt.Sprintf("related-secret-%d", i), AssigneeIDs: []uuid.UUID{member[i%2]}})
		if err != nil {
			t.Fatal(err)
		}
	}
	linkRepo := pg.NewTaskLinkRepository(env.db)
	viewRepo := pg.NewViewRepository(env.db)
	views := sprintsvc.NewViewService(viewRepo, env.sprintRepo, env.taskRepo, nil).WithTaskAccessChecker(env.taskSvc)
	v, err := views.CreateView(managerCtx, sprintdom.CreateViewInput{ProjectID: pid, Name: "private positions", ViewType: sprintdom.ViewTypeTable, ViewContext: sprintdom.ViewContextBacklog})
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range task {
		group := fmt.Sprintf("secret-group-%d", i)
		if err := views.MoveTask(managerCtx, pid, v.ID, sprintdom.MoveTaskInput{TaskID: task.ID, Position: float64(i), GroupKey: &group}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range task[1:] {
		if _, err := env.taskSvc.CreateTaskLink(managerCtx, taskdom.CreateTaskLinkInput{ProjectID: pid, SourceTaskID: task[0].ID, TargetTaskID: target.ID, LinkType: taskdom.LinkTypeBlocks}); err != nil {
			t.Fatal(err)
		}
	}
	parent := &task[1].ID
	if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[0].ID, taskdom.UpdateTaskInput{ParentTaskID: &parent}); err != nil {
		t.Fatal(err)
	}
	t.Run("parent_projection", func(t *testing.T) {
		got, err := env.taskSvc.GetTask(userCtx[0], pid, task[0].ID)
		if err != nil || got.ParentTaskID != nil {
			t.Fatalf("hidden parent returned: %+v %v", got, err)
		}
		listed, _, err := env.taskSvc.ListTasks(userCtx[0], pid, taskdom.TaskFilter{}, 10, taskdom.TaskSort{})
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range listed {
			if got.ParentTaskID != nil {
				t.Fatal("parent ID leaked in list")
			}
		}
		assigned, _, err := env.taskSvc.ListAssignedTasksInProjects(userCtx[0], []uuid.UUID{pid}, 10, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range assigned {
			if got.ParentTaskID != nil {
				t.Fatal("assigned page leaked parent")
			}
		}
		status := task[0].StatusID
		got, err = env.taskSvc.UpdateTask(userCtx[0], pid, task[0].ID, taskdom.UpdateTaskInput{StatusID: &status})
		if err != nil || got.ParentTaskID != nil {
			t.Fatalf("status response leaked parent: %+v %v", got, err)
		}

	})
	t.Run("link_and_position_sql", func(t *testing.T) {
		scope, err := authz.NewTaskAccessPolicy(pg.NewAuthzPermissionStore(env.db)).ResolveTaskAccess(userCtx[0], pid)
		if err != nil {
			t.Fatal(err)
		}
		links, err := linkRepo.ListTaskLinksVisible(userCtx[0], task[0].ID, scope)
		if err != nil || len(links) != 1 || links[0].LinkedTask.ID != task[2].ID {
			t.Errorf("SQL returned hidden links: %+v %v", links, err)
		}
		positions, err := viewRepo.ListTaskPositionsVisible(userCtx[0], v.ID, scope)
		if err != nil || len(positions) != 2 {
			t.Fatalf("SQL returned hidden positions: %+v %v", positions, err)
		}
		for _, p := range positions {
			if p.TaskID == task[1].ID || p.GroupKey != nil && *p.GroupKey == "secret-group-1" {
				t.Fatal("hidden ID/group returned")
			}
		}
	})

	t.Run("activities_current_access_and_no_side_effects", func(t *testing.T) {
		actors := [2]uuid.UUID{}
		for i := range actors {
			actors[i], _ = authz.ActorIDFromContext(userCtx[i])
		}
		comment, err := env.activitySvc.AddComment(userCtx[0], taskdom.AddCommentInput{ProjectID: pid, TaskID: task[0].ID, ActorID: actors[0], Content: json.RawMessage(`{"text":"private comment"}`)})
		if err != nil {
			t.Fatal(err)
		}
		beforeEvents, err := env.redisClient.XLen(env.ctx, events.StreamPluginEvents).Result()
		if err != nil {
			t.Fatal(err)
		}
		var beforeCount int
		if err := env.db.Get(&beforeCount, `SELECT count(*) FROM task_activities WHERE task_id=$1`, task[0].ID); err != nil {
			t.Fatal(err)
		}
		if items, err := env.activitySvc.ListActivities(userCtx[1], pid, task[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) || len(items) != 0 {
			t.Fatalf("hidden activities exposed: %+v %v", items, err)
		}
		if _, err := env.activitySvc.AddComment(userCtx[1], taskdom.AddCommentInput{ProjectID: pid, TaskID: task[0].ID, ActorID: actors[1], Content: json.RawMessage(`{"text":"forbidden @mention"}`)}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("hidden comment accepted: %v", err)
		}
		if _, err := env.activitySvc.UpdateComment(userCtx[1], comment.ID, pid, actors[1], nil, json.RawMessage(`{"text":"forbidden edit"}`)); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("hidden edit accepted: %v", err)
		}
		if err := env.activitySvc.DeleteComment(userCtx[1], comment.ID, pid, actors[1], nil); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("hidden delete accepted: %v", err)
		}
		var afterCount int
		if err := env.db.Get(&afterCount, `SELECT count(*) FROM task_activities WHERE task_id=$1`, task[0].ID); err != nil {
			t.Fatal(err)
		}
		afterEvents, err := env.redisClient.XLen(env.ctx, events.StreamPluginEvents).Result()
		if err != nil {
			t.Fatal(err)
		}
		if afterCount != beforeCount || afterEvents != beforeEvents {
			t.Fatalf("denial produced rows/events: %d/%d -> %d/%d", beforeCount, beforeEvents, afterCount, afterEvents)
		}
		replacement := []uuid.UUID{member[1]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[0].ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		if err := env.activitySvc.DeleteComment(userCtx[0], comment.ID, pid, actors[0], nil); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("former assignee still edits own comment: %v", err)
		}
		if got, err := env.activitySvc.ListActivities(userCtx[1], pid, task[0].ID); err != nil || len(got) == 0 {
			t.Fatalf("new assignee denied: %v", err)
		}
		replacement = []uuid.UUID{member[0]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[0].ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("management_refusal_and_reference_endpoints", func(t *testing.T) {
		initial, _ := linkRepo.ListTaskLinks(env.ctx, task[0].ID)
		for _, pair := range [][2]uuid.UUID{{task[0].ID, task[1].ID}, {task[1].ID, task[0].ID}} {
			if _, err := env.taskSvc.CreateTaskLink(userCtx[0], taskdom.CreateTaskLinkInput{ProjectID: pid, SourceTaskID: pair[0], TargetTaskID: pair[1], LinkType: taskdom.LinkTypeRelatesTo}); err == nil {
				t.Fatal("ordinary user created management link")
			}
		}
		if err := env.taskSvc.DeleteTaskLink(userCtx[0], pid, task[0].ID, initial[0].ID); err == nil {
			t.Fatal("ordinary user deleted link")
		}
		after, _ := linkRepo.ListTaskLinks(env.ctx, task[0].ID)
		if len(after) != len(initial) {
			t.Fatal("denied link operation mutated rows")
		}
		before, _ := viewRepo.ListTaskPositions(env.ctx, v.ID)
		if err := views.BulkMoveTasks(userCtx[0], pid, v.ID, []sprintdom.MoveTaskInput{{TaskID: task[0].ID, Position: 99}, {TaskID: task[1].ID, Position: 100}}); err == nil {
			t.Fatal("bulk move accepted ordinary write")
		}
		afterPos, _ := viewRepo.ListTaskPositions(env.ctx, v.ID)
		for i, p := range before {
			if p.Position != afterPos[i].Position {
				t.Fatal("partial bulk positions")
			}
		}
		// A narrow manager can see its own endpoint but cannot attach an unreadable parent/link.
		var role uuid.UUID
		var permissions []byte
		if err := env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE id=$1`, member[0]); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&permissions, `SELECT permissions FROM project_roles WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=permissions || '{"tasks.manage":true}'::jsonb WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]uuid.UUID{{task[0].ID, task[1].ID}, {task[1].ID, task[0].ID}} {
			if _, err := env.taskSvc.CreateTaskLink(userCtx[0], taskdom.CreateTaskLinkInput{ProjectID: pid, SourceTaskID: pair[0], TargetTaskID: pair[1], LinkType: taskdom.LinkTypeRelatesTo}); !errors.Is(err, taskdom.ErrTaskNotFound) {
				t.Fatalf("hidden endpoint accepted: %v", err)
			}
		}
		if _, err := env.taskSvc.UpdateTask(userCtx[0], pid, task[2].ID, taskdom.UpdateTaskInput{ParentTaskID: &parent}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("hidden parent accepted: %v", err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=$1 WHERE id=$2`, permissions, role); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("configuration_management_no_hidden_task_mutation", func(t *testing.T) {
		typ, err := env.taskSvc.CreateTaskType(managerCtx, taskdom.CreateTaskTypeInput{ProjectID: pid, Name: "configuration type"})
		if err != nil {
			t.Fatal(err)
		}
		status, err := env.taskSvc.CreateTaskStatus(managerCtx, taskdom.CreateTaskStatusInput{ProjectID: pid, Name: "configuration status", Category: taskdom.StatusCategoryTodo})
		if err != nil {
			t.Fatal(err)
		}
		field, err := env.taskSvc.CreateCustomFieldDefinition(managerCtx, taskdom.CreateCustomFieldDefinitionInput{ProjectID: pid, FieldKey: "secret", DisplayName: "Secret", FieldType: taskdom.FieldTypeText})
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]any{"secret": "hidden B value"}
		typeID, statusID := &typ.ID, &status.ID
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[1].ID, taskdom.UpdateTaskInput{TaskTypeID: &typeID, StatusID: &statusID, CustomFields: &values}); err != nil {
			t.Fatal(err)
		}
		calls := []func() error{
			func() error {
				_, e := env.taskSvc.CreateTaskType(userCtx[0], taskdom.CreateTaskTypeInput{ProjectID: pid, Name: "forbidden"})
				return e
			},
			func() error {
				_, e := env.taskSvc.UpdateTaskType(userCtx[0], pid, typ.ID, taskdom.UpdateTaskTypeInput{Name: "forbidden"})
				return e
			},
			func() error { return env.taskSvc.DeleteTaskType(userCtx[0], pid, typ.ID) },
			func() error { _, e := env.taskSvc.SetDefaultTaskType(userCtx[0], pid, typ.ID); return e },
			func() error {
				_, e := env.taskSvc.CreateTaskStatus(userCtx[0], taskdom.CreateTaskStatusInput{ProjectID: pid, Name: "forbidden", Category: taskdom.StatusCategoryTodo})
				return e
			},
			func() error {
				c := taskdom.StatusCategoryDone
				_, e := env.taskSvc.UpdateTaskStatus(userCtx[0], pid, status.ID, taskdom.UpdateTaskStatusInput{Category: &c})
				return e
			},
			func() error { return env.taskSvc.DeleteTaskStatus(userCtx[0], pid, status.ID) },
			func() error { _, e := env.taskSvc.SetDefaultTaskStatus(userCtx[0], pid, status.ID); return e },
			func() error { return env.taskSvc.ReorderTaskStatuses(userCtx[0], pid, []uuid.UUID{status.ID}) },
			func() error {
				_, e := env.taskSvc.CreateCustomFieldDefinition(userCtx[0], taskdom.CreateCustomFieldDefinitionInput{ProjectID: pid, FieldKey: "forbidden", DisplayName: "Forbidden", FieldType: taskdom.FieldTypeText})
				return e
			},
			func() error {
				_, e := env.taskSvc.UpdateCustomFieldDefinition(userCtx[0], pid, field.ID, taskdom.UpdateCustomFieldDefinitionInput{DisplayName: "forbidden"})
				return e
			},
			func() error { return env.taskSvc.DeleteCustomFieldDefinition(userCtx[0], pid, field.ID) },
		}
		for i, call := range calls {
			if e := call(); !errors.Is(e, taskdom.ErrTaskForbidden) {
				t.Errorf("config %d accepted ordinary write: %v", i, e)
			}
		}

		var role uuid.UUID
		var permissions []byte
		if err := env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE id=$1`, member[0]); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&permissions, `SELECT permissions FROM project_roles WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=permissions || '{"tasks.manage":true}'::jsonb WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		for i, call := range calls {
			if e := call(); !errors.Is(e, taskdom.ErrTaskForbidden) {
				t.Errorf("narrow manager config %d mutated hidden tasks: %v", i, e)
			}
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=$1 WHERE id=$2`, permissions, role); err != nil {
			t.Fatal(err)
		}
		got, err := env.taskRepo.FindTaskByID(env.ctx, task[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.TaskTypeID == nil || *got.TaskTypeID != typ.ID || got.StatusID == nil || *got.StatusID != status.ID || got.CustomFields["secret"] != "hidden B value" {
			t.Fatalf("hidden task mutated: %+v", got)
		}
		if err := env.taskSvc.DeleteCustomFieldDefinition(managerCtx, pid, field.ID); err != nil {
			t.Fatal(err)
		}
		got, _ = env.taskRepo.FindTaskByID(env.ctx, task[1].ID)
		if _, ok := got.CustomFields["secret"]; ok {
			t.Fatal("manager purge failed")
		}
	})

	t.Run("sprint_delete_preserves_all_task_relations", func(t *testing.T) {
		sp, err := env.sprintSvc.CreateSprint(managerCtx, sprintdom.CreateSprintInput{ProjectID: pid, Name: "delete boundary"})
		if err != nil {
			t.Fatal(err)
		}
		done, err := env.taskSvc.CreateTaskStatus(managerCtx, taskdom.CreateTaskStatusInput{ProjectID: pid, Name: "delete done", Category: taskdom.StatusCategoryDone})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			sid := &sp.ID
			in := taskdom.UpdateTaskInput{SprintID: &sid}
			if i == 1 {
				did := &done.ID
				in.StatusID = &did
			}
			if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[i].ID, in); err != nil {
				t.Fatal(err)
			}
		}
		if err := env.sprintSvc.DeleteSprint(userCtx[0], pid, sp.ID); !errors.Is(err, taskdom.ErrTaskForbidden) {
			t.Errorf("ordinary caller deleted sprint: %v", err)
		}

		var role uuid.UUID
		var permissions []byte
		if err := env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE id=$1`, member[0]); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&permissions, `SELECT permissions FROM project_roles WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=permissions || '{"tasks.manage":true}'::jsonb WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		if err := env.sprintSvc.DeleteSprint(userCtx[0], pid, sp.ID); !errors.Is(err, taskdom.ErrTaskForbidden) {
			t.Fatalf("narrow manager cleared hidden task sprint: %v", err)
		}
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=$1 WHERE id=$2`, permissions, role); err != nil {
			t.Fatal(err)
		}
		for _, task := range task[:2] {
			got, err := env.taskRepo.FindTaskByID(env.ctx, task.ID)
			if err != nil || got.SprintID == nil || *got.SprintID != sp.ID {
				t.Fatalf("delete changed task relation: %+v %v", got, err)
			}
		}
		if _, err := env.sprintRepo.FindSprintByID(env.ctx, sp.ID); err != nil {
			t.Fatal("denied delete removed sprint")
		}
		if err := env.sprintSvc.DeleteSprint(managerCtx, pid, sp.ID); err != nil {
			t.Fatal(err)
		}
		for _, task := range task[:2] {
			got, err := env.taskRepo.FindTaskByID(env.ctx, task.ID)
			if err != nil || got.SprintID != nil {
				t.Fatalf("manager delete failed to clear relation: %+v %v", got, err)
			}
		}
	})

	t.Run("system_activity_related_targets", func(t *testing.T) {
		repo := pg.NewTaskActivityRepository(env.db)
		seeded := map[uuid.UUID]bool{}
		bodies := []struct {
			kind    taskdom.ActivityType
			content map[string]any
		}{
			{taskdom.ActivityTypeTaskLinkAdded, map[string]any{"target_task_id": task[1].ID, "link_type": "blocks"}},
			{taskdom.ActivityTypeTaskUpdated, map[string]any{"changes": []taskdom.FieldChange{{Field: "parent_task", Old: "", New: task[1].ID.String()}}}},
			{taskdom.ActivityTypeAutomationApplied, map[string]any{"automation_name": "parent update", "parent_task_id": task[1].ID.String(), "title": task[1].Title}},
		}
		for _, body := range bodies {
			content, _ := json.Marshal(body.content)
			id := uuid.New()
			seeded[id] = true
			if err := repo.CreateActivity(env.ctx, &taskdom.Activity{ID: id, TaskID: task[0].ID, ActivityType: body.kind, Content: content, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		// User-authored text is preserved even if it contains UUID-looking strings.
		commentID := uuid.New()
		content, _ := json.Marshal(map[string]any{"text": "user text " + task[1].ID.String()})
		if err := repo.CreateActivity(env.ctx, &taskdom.Activity{ID: commentID, TaskID: task[0].ID, ActivityType: taskdom.ActivityTypeComment, Content: content, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		check := func(ctx context.Context, want int) {
			t.Helper()
			items, err := env.activitySvc.ListActivities(ctx, pid, task[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			comment := false
			for _, a := range items {
				if seeded[a.ID] {
					count++
				}
				if a.ID == commentID {
					comment = strings.Contains(string(a.Content), task[1].ID.String())
				}
			}
			if count != want || !comment {
				t.Fatalf("related activity count=%d want=%d comment unchanged=%v", count, want, comment)
			}
		}
		check(userCtx[0], 0)
		check(managerCtx, 3)
		replacement := []uuid.UUID{member[0]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[1].ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		check(userCtx[0], 3)
		replacement = []uuid.UUID{member[1]}
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[1].ID, taskdom.UpdateTaskInput{AssigneeIDs: &replacement}); err != nil {
			t.Fatal(err)
		}
		check(userCtx[0], 0)
	})
	t.Run("sprint_revoke_and_rollback", func(t *testing.T) {
		sprintSvc := sprintsvc.New(env.sprintRepo, env.taskRepo, nil).WithTaskAccessChecker(env.taskSvc)
		sp, err := sprintSvc.CreateSprint(managerCtx, sprintdom.CreateSprintInput{ProjectID: pid, Name: "atomic complete", Status: sprintdom.SprintStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		sid := &sp.ID
		if _, err := env.taskSvc.UpdateTask(managerCtx, pid, task[2].ID, taskdom.UpdateTaskInput{SprintID: &sid}); err != nil {
			t.Fatal(err)
		}
		unchanged := func() {
			t.Helper()
			got, _ := env.taskRepo.FindTaskByID(env.ctx, task[2].ID)
			s, _ := env.sprintRepo.FindSprintByID(env.ctx, sp.ID)
			if got.SprintID == nil || *got.SprintID != sp.ID || s.Status != sprintdom.SprintStatusActive {
				t.Fatalf("partial/unauthorized effects: task=%+v sprint=%+v", got, s)
			}
		}
		var role uuid.UUID
		var permissions []byte
		if err := env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, manager.ID); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&permissions, `SELECT permissions FROM project_roles WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		checker := &afterTaskCheck{checker: env.taskSvc, hook: func() {
			if _, err := env.db.Exec(`UPDATE project_roles SET permissions='{"tasks.read":true,"tasks.read_all":true,"tasks.write":true}' WHERE id=$1`, role); err != nil {
				t.Fatal(err)
			}
		}}
		revoked := sprintsvc.New(env.sprintRepo, env.taskRepo, nil).WithTaskAccessChecker(checker)
		_, err = revoked.CompleteSprint(managerCtx, pid, sp.ID, sprintdom.CompleteSprintInput{})
		if !errors.Is(err, taskdom.ErrTaskForbidden) {
			t.Errorf("revocation after precheck was not denied: %v", err)
		}
		unchanged()
		if _, err := env.db.Exec(`UPDATE project_roles SET permissions=$1 WHERE id=$2`, permissions, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`ALTER TABLE sprints ADD CONSTRAINT fail_complete CHECK(status <> 'completed')`); err != nil {
			t.Fatal(err)
		}
		if _, err := sprintSvc.CompleteSprint(managerCtx, pid, sp.ID, sprintdom.CompleteSprintInput{}); err == nil {
			t.Fatal("injected persistence failure succeeded")
		}
		unchanged()
		if _, err := env.db.Exec(`ALTER TABLE sprints DROP CONSTRAINT fail_complete`); err != nil {
			t.Fatal(err)
		}
		if _, err := sprintSvc.CompleteSprint(managerCtx, pid, sp.ID, sprintdom.CompleteSprintInput{}); err != nil {
			t.Fatal(err)
		}
	})
}

// Public writes must never establish configuration references across projects;
// preexisting malformed references must not let an A-only manager mutate B.
func TestUS005TaskConfigurationProjectBoundary(t *testing.T) {
	env := newE2EEnv(t)
	var pid [2]uuid.UUID
	var managerCtx [2]context.Context
	var clients [2]*http.Client
	var tokens [2]string
	for i, name := range []string{"ref-manager-a", "ref-manager-b"} {
		seedTaskMemberUser(t, env, name, "password1")
		clients[i], tokens[i] = taskMemberLogin(t, env, name, "password1")
		pid[i] = uuid.MustParse(createProjectForTasksViaAPI(t, env, clients[i], tokens[i]))
		u, err := env.userRepo.FindByUsername(env.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		managerCtx[i] = authz.WithActorID(env.ctx, u.ID)
	}
	seedTaskMemberUser(t, env, "ref-assignee", "password1")
	assigneeClient, assigneeToken := taskMemberLogin(t, env, "ref-assignee", "password1")
	assignee, err := env.userRepo.FindByUsername(env.ctx, "ref-assignee")
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, memberID, pid[1], assignee.ID); err != nil {
		t.Fatal(err)
	}
	var types [2]*taskdom.TaskType
	var statuses [2]*taskdom.TaskStatus
	var sprints [2]*sprintdom.Sprint
	for i := range pid {
		types[i], err = env.taskSvc.CreateTaskType(managerCtx[i], taskdom.CreateTaskTypeInput{ProjectID: pid[i], Name: "reference type"})
		if err != nil {
			t.Fatal(err)
		}
		statuses[i], err = env.taskSvc.CreateTaskStatus(managerCtx[i], taskdom.CreateTaskStatusInput{ProjectID: pid[i], Name: "reference status", Category: taskdom.StatusCategoryTodo})
		if err != nil {
			t.Fatal(err)
		}
		sprints[i], err = env.sprintSvc.CreateSprint(managerCtx[i], sprintdom.CreateSprintInput{ProjectID: pid[i], Name: "reference sprint"})
		if err != nil {
			t.Fatal(err)
		}
	}
	target, err := env.taskSvc.CreateTask(managerCtx[1], taskdom.CreateTaskInput{ProjectID: pid[1], Title: "reference target", TaskTypeID: &types[1].ID, StatusID: &statuses[1].ID, SprintID: &sprints[1].ID, AssigneeIDs: []uuid.UUID{memberID}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(t *testing.T, client *http.Client, token, method string, project uuid.UUID, suffix string, body map[string]any) int {
		t.Helper()
		req := mustRequest(env.ctx, t, method, fmt.Sprintf("%s/api/v1/projects/%s/%s", env.base, project, suffix), jsonBody(t, body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, client, req)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	snapshot := func(t *testing.T) []string {
		t.Helper()
		var out []string
		// Full persisted task state, counters, relations, activities and configuration.
		for _, table := range []string{"tasks", "task_counters", "task_assignees", "task_activities", "task_types", "task_statuses", "sprints"} {
			var rows string
			if err := env.db.Get(&rows, `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM `+table+` r`); err != nil {
				t.Fatal(err)
			}
			out = append(out, rows)
		}
		for _, stream := range []string{events.StreamTaskActivities, events.StreamTaskAssignments, events.StreamPluginEvents, events.StreamSprintActivities} {
			n, err := env.redisClient.XLen(env.ctx, stream).Result()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(n))
		}
		return out
	}
	for _, ref := range []struct {
		name    string
		foreign uuid.UUID
	}{
		{"task_type_id", types[0].ID}, {"status_id", statuses[0].ID}, {"sprint_id", sprints[0].ID},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			t.Run(method+"_foreign_"+ref.name, func(t *testing.T) {
				before := snapshot(t)
				suffix := "tasks"
				if method == http.MethodPatch {
					suffix += "/" + target.ID.String()
				}
				code := request(t, clients[1], tokens[1], method, pid[1], suffix, map[string]any{"title": "forbidden foreign reference", ref.name: ref.foreign, "assignee_ids": []uuid.UUID{memberID}})
				if code != http.StatusNotFound {
					t.Errorf("foreign reference returned HTTP %d, want 404", code)
				}
				if !reflect.DeepEqual(before, snapshot(t)) {
					t.Error("refused reference changed rows or emitted events")
				}
			})
		}
	}
	t.Run("assignee_status_only_foreign", func(t *testing.T) {
		before := snapshot(t)
		code := request(t, assigneeClient, assigneeToken, http.MethodPatch, pid[1], "tasks/"+target.ID.String(), map[string]any{"status_id": statuses[0].ID})
		if code != http.StatusNotFound {
			t.Errorf("foreign progress status returned HTTP %d, want 404", code)
		}
		if !reflect.DeepEqual(before, snapshot(t)) {
			t.Error("refused progress changed rows or emitted events")
		}
	})
	t.Run("same_project_public_writes", func(t *testing.T) {
		body := map[string]any{"title": "valid references", "task_type_id": types[1].ID, "status_id": statuses[1].ID, "sprint_id": sprints[1].ID, "assignee_ids": []uuid.UUID{memberID}}
		if code := request(t, clients[1], tokens[1], http.MethodPost, pid[1], "tasks", body); code != http.StatusCreated {
			t.Fatalf("same-project create: %d", code)
		}
		if code := request(t, clients[1], tokens[1], http.MethodPatch, pid[1], "tasks/"+target.ID.String(), body); code != http.StatusOK {
			t.Fatalf("same-project management patch: %d", code)
		}
		progress, err := env.taskSvc.CreateTaskStatus(managerCtx[1], taskdom.CreateTaskStatusInput{ProjectID: pid[1], Name: "same project progress", Category: taskdom.StatusCategoryDone})
		if err != nil {
			t.Fatal(err)
		}
		if code := request(t, assigneeClient, assigneeToken, http.MethodPatch, pid[1], "tasks/"+target.ID.String(), map[string]any{"status_id": progress.ID}); code != http.StatusOK {
			t.Fatalf("same-project progress patch: %d", code)
		}
		got, err := env.taskRepo.FindTaskByID(env.ctx, target.ID)
		if err != nil || got.Title != "valid references" || got.StatusID == nil || *got.StatusID != progress.ID || got.TaskTypeID == nil || *got.TaskTypeID != types[1].ID || got.SprintID == nil || *got.SprintID != sprints[1].ID {
			t.Fatalf("same-project writes did not persist: %+v %v", got, err)
		}
		if code := request(t, clients[1], tokens[1], http.MethodPatch, pid[1], "tasks/"+target.ID.String(), map[string]any{"task_type_id": nil, "status_id": nil, "sprint_id": nil}); code != http.StatusOK {
			t.Fatalf("clear optional references: %d", code)
		}
		got, err = env.taskRepo.FindTaskByID(env.ctx, target.ID)
		if err != nil || got.TaskTypeID != nil || got.StatusID != nil || got.SprintID != nil {
			t.Fatalf("clear did not persist: %+v %v", got, err)
		}
	})
	// A previously had B access; revoke it before exercising legacy B -> A rows.
	actorA, _ := authz.ActorIDFromContext(managerCtx[0])
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), pid[1], actorA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, pid[1], actorA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.taskSvc.GetTask(managerCtx[0], pid[1], target.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatalf("A retained B task access: %v", err)
	}
	legacyCalls := []struct {
		name string
		call func() error
	}{
		{"delete_sprint", func() error { return env.sprintSvc.DeleteSprint(managerCtx[0], pid[0], sprints[0].ID) }},
		{"delete_type", func() error { return env.taskSvc.DeleteTaskType(managerCtx[0], pid[0], types[0].ID) }},
		{"delete_status", func() error { return env.taskSvc.DeleteTaskStatus(managerCtx[0], pid[0], statuses[0].ID) }},
		{"change_status_category", func() error {
			c := taskdom.StatusCategoryDone
			_, e := env.taskSvc.UpdateTaskStatus(managerCtx[0], pid[0], statuses[0].ID, taskdom.UpdateTaskStatusInput{Category: &c})
			return e
		}},
		{"update_type", func() error {
			_, e := env.taskSvc.UpdateTaskType(managerCtx[0], pid[0], types[0].ID, taskdom.UpdateTaskTypeInput{Name: "changed type"})
			return e
		}},
		{"update_sprint", func() error {
			_, e := env.sprintSvc.UpdateSprint(managerCtx[0], pid[0], sprints[0].ID, sprintdom.UpdateSprintInput{Name: "changed sprint"})
			return e
		}},
		{"complete_sprint", func() error {
			_, e := env.sprintSvc.CompleteSprint(managerCtx[0], pid[0], sprints[0].ID, sprintdom.CompleteSprintInput{})
			return e
		}},
	}
	for _, test := range legacyCalls {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy_%s/deleted=%t", test.name, deleted), func(t *testing.T) {
				// Each regression has its own configuration to keep a RED delete from
				// destroying the fixture needed by later cases.
				typ, e := env.taskSvc.CreateTaskType(managerCtx[0], taskdom.CreateTaskTypeInput{ProjectID: pid[0], Name: uuid.NewString()})
				if e != nil {
					t.Fatal(e)
				}
				st, e := env.taskSvc.CreateTaskStatus(managerCtx[0], taskdom.CreateTaskStatusInput{ProjectID: pid[0], Name: uuid.NewString(), Category: taskdom.StatusCategoryTodo})
				if e != nil {
					t.Fatal(e)
				}
				sp, e := env.sprintSvc.CreateSprint(managerCtx[0], sprintdom.CreateSprintInput{ProjectID: pid[0], Name: uuid.NewString()})
				if e != nil {
					t.Fatal(e)
				}
				types[0], statuses[0], sprints[0] = typ, st, sp
				if _, err := env.db.Exec(`UPDATE tasks SET task_type_id=$1,status_id=$2,sprint_id=$3,deleted_at=CASE WHEN $5 THEN now() ELSE NULL END WHERE id=$4`, typ.ID, st.ID, sp.ID, target.ID, deleted); err != nil {
					t.Fatal(err)
				}
				before := snapshot(t)
				if err := test.call(); !errors.Is(err, taskdom.ErrTaskForbidden) {
					t.Errorf("A-only manager changed foreign task interpretation: %v", err)
				}
				if !reflect.DeepEqual(before, snapshot(t)) {
					t.Error("legacy guard changed foreign task/configuration or emitted events")
				}
				// Explicit administrator fixture repair, never part of a production refusal.
				if _, err := env.db.Exec(`UPDATE tasks SET task_type_id=$1,status_id=$2,sprint_id=$3,deleted_at=NULL WHERE id=$4`, types[1].ID, statuses[1].ID, sprints[1].ID, target.ID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	t.Run("same_project_configuration_mutations", func(t *testing.T) {
		c := taskdom.StatusCategoryDone
		if _, err := env.taskSvc.UpdateTaskStatus(managerCtx[1], pid[1], statuses[1].ID, taskdom.UpdateTaskStatusInput{Category: &c}); err != nil {
			t.Fatal(err)
		}
		if err := env.sprintSvc.DeleteSprint(managerCtx[1], pid[1], sprints[1].ID); err != nil {
			t.Fatal(err)
		}
		if err := env.taskSvc.DeleteTaskType(managerCtx[1], pid[1], types[1].ID); err != nil {
			t.Fatal(err)
		}
		if err := env.taskSvc.DeleteTaskStatus(managerCtx[1], pid[1], statuses[1].ID); err != nil {
			t.Fatal(err)
		}
		got, err := env.taskRepo.FindTaskByID(env.ctx, target.ID)
		if err != nil || got.TaskTypeID != nil || got.StatusID != nil || got.SprintID != nil {
			t.Fatalf("same-project cleanup failed: %+v %v", got, err)
		}
	})
}

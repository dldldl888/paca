package e2e_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	sprintsvc "github.com/Paca-AI/api/internal/service/sprint"
	"github.com/google/uuid"
)

// Exercises the actual sqlx view-position projection: losing a nullable creator,
// position or assignee must fail independently of the ordinary task-list scan.
func TestTaskViewPositionScanAndBoundaries(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "position-manager", "password1")
	client, token := taskMemberLogin(t, env, "position-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, err := env.userRepo.FindByUsername(env.ctx, "position-manager")
	if err != nil {
		t.Fatal(err)
	}
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "position-reader", "password1", "Position reader")
	reader, err := env.userRepo.FindByUsername(env.ctx, "position-reader")
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, memberID, pid, reader.ID); err != nil {
		t.Fatal(err)
	}
	readerCtx := authz.WithActorID(env.ctx, reader.ID)
	viewRepo := pg.NewViewRepository(env.db)
	views := sprintsvc.NewViewService(viewRepo, env.sprintRepo, env.taskRepo, nil).WithTaskAccessChecker(env.taskSvc)
	view, err := views.CreateView(managerCtx, sprintdom.CreateViewInput{ProjectID: pid, Name: "Position scan", ViewType: sprintdom.ViewTypeTable, ViewContext: sprintdom.ViewContextBacklog})
	if err != nil {
		t.Fatal(err)
	}
	var tasks [6]*taskdom.Task
	positions := []float64{20, 10, 0, 0, 5, 1}
	for i := range tasks {
		created := time.Date(2026, 9, 1, 0, i, 0, 0, time.UTC)
		tasks[i] = &taskdom.Task{ID: uuid.New(), ProjectID: pid, Title: fmt.Sprintf("position-%d", i), CreatorUserID: &reader.ID, Importance: i, Tags: []string{"scan"}, CustomFields: map[string]any{"score": float64(i)}, Description: json.RawMessage(`{"text":"scan body"}`), CreatedAt: created, UpdatedAt: created}
		if i == 1 || i == 3 {
			tasks[i].CreatorUserID = nil // legacy rows remain readable through assignment
			tasks[i].AssigneeIDs = []uuid.UUID{memberID}
		}
		if i == 4 {
			tasks[i].CreatorUserID = &manager.ID // hidden despite an earlier saved position
		}
		if err := env.taskRepo.CreateTask(env.ctx, tasks[i]); err != nil {
			t.Fatal(err)
		}
		if i != 2 && i != 3 {
			if err := views.MoveTask(managerCtx, pid, view.ID, sprintdom.MoveTaskInput{TaskID: tasks[i].ID, Position: positions[i]}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := env.db.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, tasks[5].ID); err != nil {
		t.Fatal(err)
	}

	t.Run("projection_and_cursor", func(t *testing.T) {
		sort := taskdom.TaskSort{By: "view_position", ViewID: &view.ID}
		filter := taskdom.TaskFilter{}
		// Three pages cross both the non-NULL and NULL cursor branches. Hidden
		// and deleted rows cannot displace the visible page or inflate hasMore.
		for page, indexes := range [][]int{{1, 0}, {2}, {3}} {
			got, more, err := env.taskSvc.ListTasks(readerCtx, pid, filter, len(indexes), sort)
			if err != nil || len(got) != len(indexes) || more != (page < 2) {
				t.Fatalf("page %d: tasks=%v more=%v err=%v", page, got, more, err)
			}
			for n, index := range indexes {
				want, item := tasks[index], got[n]
				if item.ID != want.ID || item.ProjectID != pid || item.TaskNumber != want.TaskNumber || item.Title != want.Title || item.Importance != want.Importance || !reflect.DeepEqual(item.CreatorUserID, want.CreatorUserID) || !reflect.DeepEqual(item.Tags, want.Tags) || !reflect.DeepEqual(item.CustomFields, want.CustomFields) || !item.CreatedAt.Equal(want.CreatedAt) || !item.UpdatedAt.Equal(want.UpdatedAt) {
					t.Fatalf("page %d lost task fields: got=%+v want=%+v", page, item, want)
				}
				var description map[string]string
				if err := json.Unmarshal(item.Description, &description); err != nil || description["text"] != "scan body" {
					t.Fatalf("description=%s err=%v", item.Description, err)
				}
				if len(item.AssigneeIDs) != len(want.AssigneeIDs) || len(want.AssigneeIDs) == 1 && item.AssigneeIDs[0] != memberID {
					t.Fatalf("assignee hydration: got=%v want=%v", item.AssigneeIDs, want.AssigneeIDs)
				}
				if index < 2 {
					if item.ViewPosition == nil || *item.ViewPosition != positions[index] {
						t.Fatalf("position=%v want=%v", item.ViewPosition, positions[index])
					}
				} else if item.ViewPosition != nil {
					t.Fatalf("NULL position became %v", *item.ViewPosition)
				}
			}
			cursor := taskdom.EncodeTaskCursor(got[len(got)-1], sort)
			filter.CursorAfter = &cursor
		}
	})

	t.Run("configured_checker_rejects_foreign_task_without_partial_write", func(t *testing.T) {
		foreignPID := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
		foreign, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: foreignPID, Title: "foreign position"})
		if err != nil {
			t.Fatal(err)
		}
		before, err := viewRepo.ListTaskPositions(env.ctx, view.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := views.MoveTask(managerCtx, pid, view.ID, sprintdom.MoveTaskInput{TaskID: foreign.ID, Position: 99}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("foreign move accepted: %v", err)
		}
		if err := views.BulkMoveTasks(managerCtx, pid, view.ID, []sprintdom.MoveTaskInput{{TaskID: tasks[0].ID, Position: 99}, {TaskID: foreign.ID, Position: 100}}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("foreign bulk move accepted: %v", err)
		}
		after, err := viewRepo.ListTaskPositions(env.ctx, view.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("denied move persisted changes: before=%+v after=%+v err=%v", before, after, err)
		}
	})
}

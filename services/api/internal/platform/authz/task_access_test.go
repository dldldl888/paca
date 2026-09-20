package authz_test

import (
	"context"
	"errors"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"testing"
)

type taskActorStore struct {
	actors map[uuid.UUID]map[uuid.UUID]authz.TaskActorAccess
}

func (s *taskActorStore) FindTaskActorAccess(_ context.Context, userID, projectID uuid.UUID) (authz.TaskActorAccess, error) {
	actor, ok := s.actors[userID][projectID]
	if !ok {
		return authz.TaskActorAccess{}, taskdom.ErrTaskNotFound
	}
	return actor, nil
}

func TestUS002LiveTaskPolicyScopeAndPermissions(t *testing.T) {
	user, project, other, member := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	store := &taskActorStore{actors: map[uuid.UUID]map[uuid.UUID]authz.TaskActorAccess{user: {project: {UserID: user, MemberID: &member, ProjectPermissions: []authz.Permission{authz.PermissionTasksAll}}, other: {UserID: user}}}}
	policy := authz.NewTaskAccessPolicy(store)
	ctx := authz.WithActorID(context.Background(), user)
	scope, err := policy.ResolveTaskAccess(ctx, project)
	if err != nil || !scope.ReadAll || !scope.Manage || !scope.Write {
		t.Fatalf("manager scope: %+v %v", scope, err)
	}
	if _, err := policy.ResolveTaskAccess(ctx, other); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("project manager crossed project scope")
	}
	store.actors[user][project] = authz.TaskActorAccess{UserID: user, MemberID: &member, ProjectPermissions: []authz.Permission{authz.PermissionTasksRead, authz.PermissionTasksWrite}}
	scope, err = policy.ResolveTaskAccess(ctx, project)
	if err != nil || scope.ReadAll || scope.Manage {
		t.Fatalf("demotion retained management: %+v %v", scope, err)
	}
	for _, permissions := range [][]authz.Permission{{authz.PermissionTasksReadAll, authz.PermissionTasksManage}, {authz.PermissionTasksWrite}} {
		store.actors[user][project] = authz.TaskActorAccess{UserID: user, MemberID: &member, ProjectPermissions: permissions}
		if _, err := policy.ResolveTaskAccess(ctx, project); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("missing read bypassed: %v", err)
		}
	}
	store.actors[user][project] = authz.TaskActorAccess{UserID: user, GlobalPermissions: []authz.Permission{authz.PermissionTasksAll}}
	if _, err := policy.ResolveTaskAccess(ctx, project); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("global tasks.* supplied missing project qualification")
	}
	store.actors[user][project] = authz.TaskActorAccess{UserID: user, GlobalPermissions: []authz.Permission{authz.PermissionAll}}
	scope, err = policy.ResolveTaskAccess(ctx, project)
	if err != nil || !scope.ReadAll || !scope.Manage {
		t.Fatal("current global wildcard did not authorize project")
	}
	if _, err := policy.ResolveTaskAccess(authz.WithAgentCredential(ctx), project); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("shared agent credential became global human admin")
	}
	delete(store.actors[user], project)
	if _, err := policy.ResolveTaskAccess(ctx, project); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("deleted user retained access")
	}
}

func TestUS004TaskProgressAndBoundedServiceGrant(t *testing.T) {
	project, user, member, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	task := &taskdom.Task{ID: uuid.New(), ProjectID: project, CreatorUserID: &user, AssigneeIDs: []uuid.UUID{member}}
	scope := taskdom.AccessScope{ProjectID: project, UserID: user, MemberID: other, Write: true}
	if err := scope.Require(task, taskdom.AccessRead); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatalf("creator-only read: %v", err)
	}
	if err := scope.Require(task, taskdom.AccessProgress); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatalf("creator-only progress: %v", err)
	}
	scope.MemberID = member
	if err := scope.Require(task, taskdom.AccessProgress); err != nil {
		t.Fatal(err)
	}
	if err := scope.Require(task, taskdom.AccessManage); !errors.Is(err, taskdom.ErrTaskForbidden) {
		t.Fatal("assignee can manage")
	}
	scope.Manage = true
	scope.Write = false
	if err := scope.Require(task, taskdom.AccessManage); !errors.Is(err, taskdom.ErrTaskForbidden) {
		t.Fatal("manage bypassed tasks.write")
	}
	ids := []uuid.UUID{task.ID}
	ctx, err := authz.WithTaskServiceGrant(context.Background(), authz.TaskServiceGrant{Name: "automation", ProjectID: project, TaskIDs: ids, Action: taskdom.AccessProgress})
	if err != nil {
		t.Fatal(err)
	}
	ids[0] = other
	grant, err := authz.NewTaskAccessPolicy(nil).ResolveTaskAccess(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.Require(task, taskdom.AccessProgress); err != nil {
		t.Fatalf("bounded progress denied: %v", err)
	}
	if err := grant.Require(task, taskdom.AccessManage); !errors.Is(err, taskdom.ErrTaskForbidden) {
		t.Fatal("progress grant acquired management")
	}
	sibling := *task
	sibling.ID = other
	if err := grant.Require(&sibling, taskdom.AccessRead); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("grant read sibling")
	}
	if _, err := authz.WithTaskServiceGrant(context.Background(), authz.TaskServiceGrant{Name: "automation", ProjectID: project}); err == nil {
		t.Fatal("empty backend scope allowed")
	}
	if _, err := authz.NewTaskAccessPolicy(nil).ResolveTaskAccess(ctx, uuid.New()); !errors.Is(err, taskdom.ErrTaskNotFound) {
		t.Fatal("grant crossed project")
	}
}

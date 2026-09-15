package authz

import (
	"context"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/google/uuid"
	"strings"
)

type TaskActorAccess struct {
	UserID             uuid.UUID
	MemberID           *uuid.UUID
	GlobalPermissions  []Permission
	ProjectPermissions []Permission
}
type TaskAccessStore interface {
	FindTaskActorAccess(context.Context, uuid.UUID, uuid.UUID) (TaskActorAccess, error)
}
type TaskAccessPolicy struct{ store TaskAccessStore }

func NewTaskAccessPolicy(store TaskAccessStore) *TaskAccessPolicy {
	return &TaskAccessPolicy{store: store}
}

// TaskServiceGrant is trusted backend context, never HTTP input. It confers no
// private-conversation authority and cannot create arbitrary new tasks.
type TaskServiceGrant struct {
	Name      string
	ProjectID uuid.UUID
	TaskIDs   []uuid.UUID
	Action    taskdom.AccessAction
}
type taskGrantKey struct{}

func WithTaskServiceGrant(ctx context.Context, grant TaskServiceGrant) (context.Context, error) {
	if strings.TrimSpace(grant.Name) == "" || grant.ProjectID == uuid.Nil || len(grant.TaskIDs) == 0 || grant.Action > taskdom.AccessManage {
		return nil, taskdom.ErrTaskForbidden
	}
	for _, id := range grant.TaskIDs {
		if id == uuid.Nil {
			return nil, taskdom.ErrTaskForbidden
		}
	}
	grant.TaskIDs = append([]uuid.UUID(nil), grant.TaskIDs...)
	return context.WithValue(ctx, taskGrantKey{}, grant), nil
}

func (p *TaskAccessPolicy) ResolveTaskAccess(ctx context.Context, projectID uuid.UUID) (taskdom.AccessScope, error) {
	denied := taskdom.AccessScope{}
	if grant, ok := ctx.Value(taskGrantKey{}).(TaskServiceGrant); ok {
		if grant.ProjectID != projectID || len(grant.TaskIDs) == 0 {
			return denied, taskdom.ErrTaskNotFound
		}
		return taskdom.AccessScope{ProjectID: projectID, ReadAll: true, Write: grant.Action >= taskdom.AccessProgress, Manage: grant.Action == taskdom.AccessManage, TaskIDs: append([]uuid.UUID(nil), grant.TaskIDs...)}, nil
	}
	// An agent's shared bot account is not a human delegation.
	if _, ok := AgentIDFromContext(ctx); ok || IsAgentCredential(ctx) {
		return denied, taskdom.ErrTaskNotFound
	}
	userID, ok := ActorIDFromContext(ctx)
	if !ok || projectID == uuid.Nil || p == nil || p.store == nil {
		return denied, taskdom.ErrTaskNotFound
	}
	actor, err := p.store.FindTaskActorAccess(ctx, userID, projectID)
	if err != nil {
		return denied, err
	}
	if actor.UserID != userID {
		return denied, taskdom.ErrTaskNotFound
	}
	granted := map[Permission]struct{}{}
	globalAdmin := false
	for _, permission := range actor.GlobalPermissions {
		granted[permission] = struct{}{}
		if permission == PermissionAll {
			globalAdmin = true
		}
	}
	if actor.MemberID == nil && !globalAdmin {
		return denied, taskdom.ErrTaskNotFound
	}
	if actor.MemberID != nil {
		for _, permission := range actor.ProjectPermissions {
			granted[permission] = struct{}{}
		}
	}
	if !hasPermission(granted, PermissionTasksRead) {
		return denied, taskdom.ErrTaskNotFound
	}
	scope := taskdom.AccessScope{ProjectID: projectID, UserID: userID, ReadAll: hasPermission(granted, PermissionTasksReadAll), Write: hasPermission(granted, PermissionTasksWrite), Manage: hasPermission(granted, PermissionTasksManage)}
	if actor.MemberID != nil {
		scope.MemberID = *actor.MemberID
	}
	return scope, nil
}

package agentsvc

import (
	"context"
	"errors"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"testing"
)

type agentTaskAccessFixture struct {
	scope taskdom.AccessScope
	task  *taskdom.Task
}

func (f agentTaskAccessFixture) ResolveTaskAccess(ctx context.Context, pid uuid.UUID) (taskdom.AccessScope, error) {
	id, ok := authz.ActorIDFromContext(ctx)
	if !ok || id != f.scope.UserID || pid != f.scope.ProjectID {
		return taskdom.AccessScope{}, taskdom.ErrTaskNotFound
	}
	return f.scope, nil
}
func (f agentTaskAccessFixture) CheckTaskAccess(ctx context.Context, pid, id uuid.UUID, action taskdom.AccessAction) (*taskdom.Task, error) {
	scope, err := f.ResolveTaskAccess(ctx, pid)
	if err != nil {
		return nil, err
	}
	if f.task == nil || f.task.ID != id {
		return nil, taskdom.ErrTaskNotFound
	}
	if err := scope.Require(f.task, action); err != nil {
		return nil, err
	}
	return f.task, nil
}
func agentTaskFixture(pid, tid uuid.UUID) agentTaskAccessFixture {
	return agentTaskAccessFixture{scope: taskdom.AccessScope{ProjectID: pid, UserID: uuid.New(), MemberID: uuid.New(), ReadAll: true, Write: true, Manage: true}, task: &taskdom.Task{ID: tid, ProjectID: pid}}
}

func TestDescriptionWriteRequiresManageBeforeSideEffects(t *testing.T) {
	pid, tid, aid := uuid.New(), uuid.New(), uuid.New()
	for _, mode := range []string{"missing_checker", "missing_actor", "assignee", "creator", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			access := agentTaskFixture(pid, tid)
			svc := New(&mockAgentRepo{findVisibleAgentInProject: func(context.Context, uuid.UUID, uuid.UUID) (*agentdom.Agent, error) {
				t.Fatal("agent lookup preceded task refusal")
				return nil, nil
			}}, &mockProjectRepo{}, nil, &mockPluginRepo{}).WithTaskAccess(access, access)
			ctx := authz.WithActorID(context.Background(), access.scope.UserID)
			switch mode {
			case "missing_checker":
				svc.taskChecker = nil
			case "missing_actor":
				ctx = context.Background()
			case "assignee":
				access.scope.ReadAll = false
				access.scope.Manage = false
				access.task.AssigneeIDs = []uuid.UUID{access.scope.MemberID}
				svc.WithTaskAccess(access, access)
			case "creator":
				access.scope.ReadAll = false
				access.scope.Manage = false
				access.task.CreatorUserID = &access.scope.UserID
				svc.WithTaskAccess(access, access)
			case "foreign":
				access.task.ProjectID = uuid.New()
			}
			if conv, err := svc.TriggerDescriptionWrite(ctx, pid, aid, tid, uuid.New()); err == nil || conv != nil {
				t.Fatalf("denied request returned conversation %v err=%v", conv, err)
			}
		})
	}
}

func TestDescriptionWriteAttributesCurrentMemberAndPropagatesDispatchFailure(t *testing.T) {
	pid, tid, aid := uuid.New(), uuid.New(), uuid.New()
	access := agentTaskFixture(pid, tid)
	failure := errors.New("dispatch unavailable")
	creates := 0
	repo := &mockAgentRepo{
		findVisibleAgentInProject: func(context.Context, uuid.UUID, uuid.UUID) (*agentdom.Agent, error) {
			return &agentdom.Agent{ID: aid, ProjectID: pid}, nil
		},
		findAgentByID: func(context.Context, uuid.UUID) (*agentdom.Agent, error) { return nil, failure },
		createConversation: func(_ context.Context, c *agentdom.AgentConversation) error {
			creates++
			if c.TriggeredByMemberID == nil || *c.TriggeredByMemberID != access.scope.MemberID || c.TaskID == nil || *c.TaskID != tid {
				t.Fatalf("forged attribution: %+v", c)
			}
			return nil
		},
	}
	svc := New(repo, &mockProjectRepo{}, nil, &mockPluginRepo{}).WithTaskAccess(access, access)
	conv, err := svc.TriggerDescriptionWrite(authz.WithActorID(context.Background(), access.scope.UserID), pid, aid, tid, uuid.New())
	if !errors.Is(err, failure) || conv != nil || creates != 1 {
		t.Fatalf("conv=%v err=%v creates=%d", conv, err, creates)
	}
}

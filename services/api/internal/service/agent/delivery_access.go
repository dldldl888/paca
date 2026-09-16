package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	annotationdom "github.com/Paca-AI/api/internal/domain/annotation"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/google/uuid"
)

type deliveryRepository interface {
	PrivateHistoryOwner(context.Context, uuid.UUID) (uuid.UUID, error)
	ListDeliveryUsers(context.Context) ([]uuid.UUID, error)
	ListPrivateRunIDs(context.Context) ([]uuid.UUID, error)
	FailRevokedRun(context.Context, uuid.UUID) error
	DeliveryActivityVisible(context.Context, string, []byte, taskdom.AccessScope) (bool, error)
}

func (s *Service) WithDeliveryGate(g *delivery.Gate) *Service { s.deliveryGate = g; return s }
func (s *Service) enterDelivery(ctx context.Context) (context.Context, string, func(), error) {
	if s.deliveryGate == nil || database.Transaction(ctx) != nil {
		return ctx, "", func() {}, nil
	}
	return s.deliveryGate.Enter(ctx)
}
func RunDenied(err error) bool {
	for _, refusal := range []error{agentdom.ErrConversationNotFound, agentdom.ErrChatSessionNotFound, agentdom.ErrAgentNotFound, taskdom.ErrTaskNotFound, taskdom.ErrTaskForbidden, docdom.ErrDocNotFound, annotationdom.ErrAnnotationNotFound, automationdom.ErrNotFound} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}
func (s *Service) ReconcilePrivateRuns(ctx context.Context) error {
	repo, ok := s.repo.(deliveryRepository)
	if !ok {
		return errors.New("run reconciliation unavailable")
	}
	ids, err := repo.ListPrivateRunIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		c, err := s.repo.FindConversationByID(ctx, id)
		if err != nil {
			return err
		}
		_, err = s.AuthorizeRun(ctx, c.ID, c.AgentID, false)
		if err == nil {
			continue
		}
		if !RunDenied(err) {
			return err
		}
		if err = repo.FailRevokedRun(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

type RealtimeScope struct {
	EventType       string          `json:"event_type"`
	ProjectID       string          `json:"project_id,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	RecipientUserID string          `json:"recipient_user_id,omitempty"`
	ActivityID      string          `json:"activity_id,omitempty"`
	ActivityType    string          `json:"activity_type,omitempty"`
	Content         json.RawMessage `json:"content,omitempty"`
}
type RealtimeRecipients struct {
	Epoch   string   `json:"epoch"`
	UserIDs []string `json:"user_ids"`
}

func (s *Service) CurrentRecipients(ctx context.Context, in RealtimeScope) (*RealtimeRecipients, error) {
	ctx, epoch, leave, err := s.enterDelivery(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	out := &RealtimeRecipients{Epoch: epoch, UserIDs: []string{}}
	if s.deliveryGate == nil || s.authorizer == nil {
		return out, nil
	}
	repo, ok := s.repo.(deliveryRepository)
	if !ok {
		return nil, errors.New("recipient repository unavailable")
	}
	var project, task uuid.UUID
	if in.ProjectID != "" {
		project, err = uuid.Parse(in.ProjectID)
		if err != nil || project == uuid.Nil {
			return out, nil
		}
	}
	if in.TaskID != "" {
		task, err = uuid.Parse(in.TaskID)
		if err != nil || task == uuid.Nil {
			return out, nil
		}
	}
	var candidates []uuid.UUID
	var conversation *agentdom.AgentConversation
	if in.ConversationID != "" {
		cid, e := uuid.Parse(in.ConversationID)
		if e != nil || cid == uuid.Nil {
			return out, nil
		}
		conversation, err = s.repo.FindConversationByID(ctx, cid)
		if err != nil {
			if RunDenied(err) {
				return out, nil
			}
			return nil, err
		}
		if in.ProjectID != "" && project != conversation.ProjectID {
			return out, nil
		}
		project = conversation.ProjectID
		if in.TaskID != "" && (conversation.TaskID == nil || task != *conversation.TaskID) {
			return out, nil
		}
		if conversation.TaskID != nil {
			task = *conversation.TaskID
		}
		if in.EventType == "agent.authorization_revoked" {
			owner, e := repo.PrivateHistoryOwner(ctx, conversation.ID)
			if e != nil {
				if RunDenied(e) {
					return out, nil
				}
				return nil, e
			}
			out.UserIDs = []string{owner.String()}
			return out, nil
		}
		if conversation.Audience == agentdom.AudienceOwnerPrivate {
			a, e := s.AuthorizeRun(ctx, conversation.ID, conversation.AgentID, false)
			if e != nil {
				if RunDenied(e) {
					return out, nil
				}
				return nil, e
			}
			candidates = []uuid.UUID{uuid.MustParse(a.OwnerUserID)}
		}
	}
	typedActivity := in.ActivityType != "" || in.ActivityID != "" || in.EventType == "task.updated" || in.EventType == "task.link.added" || in.EventType == "automation.applied" || strings.HasPrefix(in.EventType, "task.activity.")
	permission := authz.Permission("")
	switch {
	case typedActivity:
		if task == uuid.Nil {
			return out, nil
		}
	case strings.HasPrefix(in.EventType, "notification."):
		uid, e := uuid.Parse(in.RecipientUserID)
		if e != nil || uid == uuid.Nil {
			return out, nil
		}
		candidates = []uuid.UUID{uid}
	case strings.HasPrefix(in.EventType, "agent."):
		if conversation == nil {
			return out, nil
		}
		permission = authz.PermissionConversationsRead
	case strings.HasPrefix(in.EventType, "task."), strings.HasPrefix(in.EventType, "github."), in.EventType == "workflow.assigned":
		if task == uuid.Nil {
			return out, nil
		}
	case strings.HasPrefix(in.EventType, "doc."):
		permission = authz.PermissionDocsRead
	case strings.HasPrefix(in.EventType, "workflow."), strings.HasPrefix(in.EventType, "automation."):
		permission = authz.PermissionWorkflowsRead
	case strings.HasPrefix(in.EventType, "sprint."), strings.HasPrefix(in.EventType, "view."):
		permission = authz.PermissionSprintsRead
	case strings.HasPrefix(in.EventType, "environment."):
		permission = authz.PermissionEnvironmentsRead
	default:
		return out, nil
	}
	if project == uuid.Nil && (conversation == nil || conversation.Audience != agentdom.AudienceOwnerPrivate) {
		return out, nil
	}
	allUsers, e := repo.ListDeliveryUsers(ctx)
	if e != nil {
		return nil, e
	}
	active := make(map[uuid.UUID]bool, len(allUsers))
	for _, u := range allUsers {
		active[u] = true
	}
	if candidates == nil {
		candidates = allUsers
	}
	for _, user := range candidates {
		if !active[user] {
			continue
		}
		actor := authz.WithActorID(ctx, user)
		if task != uuid.Nil {
			if s.taskChecker == nil {
				continue
			}
			if _, e := s.taskChecker.CheckTaskAccess(actor, project, task, taskdom.AccessRead); e != nil {
				if RunDenied(e) {
					continue
				}
				return nil, e
			}
		}
		if permission != "" && project != uuid.Nil {
			allowed, e := s.authorizer.HasPermissions(actor, user, &project, "", permission)
			if e != nil {
				return nil, e
			}
			if !allowed {
				continue
			}
		}
		// Typed activity metadata must be present even before its asynchronous row insert.
		if typedActivity {
			if task == uuid.Nil || in.ActivityType == "" || len(in.Content) == 0 {
				continue
			}
			if !strings.HasPrefix(in.EventType, "task.activity.") && !strings.HasPrefix(in.EventType, "task.comment.") && in.ActivityType != in.EventType {
				continue
			}
			content := in.Content
			var text string
			if json.Unmarshal(content, &text) == nil {
				content = []byte(text)
			}
			var object map[string]json.RawMessage
			if json.Unmarshal(content, &object) != nil || object == nil {
				continue
			}
			resolver, ok := s.taskChecker.(interface {
				ResolveTaskAccess(context.Context, uuid.UUID) (taskdom.AccessScope, error)
			})
			if !ok {
				return nil, errors.New("task scope unavailable")
			}
			scope, e := resolver.ResolveTaskAccess(actor, project)
			if e != nil {
				if RunDenied(e) {
					continue
				}
				return nil, e
			}
			allowed, e := repo.DeliveryActivityVisible(actor, in.ActivityType, content, scope)
			if e != nil {
				return nil, e
			}
			if !allowed {
				continue
			}
		}
		out.UserIDs = append(out.UserIDs, user.String())
	}
	return out, nil
}

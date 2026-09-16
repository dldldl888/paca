package agentsvc

import (
	"context"
	"errors"
	"time"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	annotationdom "github.com/Paca-AI/api/internal/domain/annotation"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
)

type runRepository interface {
	CurrentChatActor(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (uuid.UUID, error)
	MergeConversationContext(context.Context, uuid.UUID, []agentdom.ContextItemRef) error
	BeginConversationTurn(context.Context, uuid.UUID, uuid.UUID, bool) (bool, error)
}

// WithPrivateRuns uses existing object readers and the task policy; no role engine
// is duplicated here. A nil instance preserves legacy shared ACP operation.
func (s *Service) WithPrivateRuns(instance uuid.UUID, repo runRepository, docs docdom.DocumentService, annotations annotationdom.Service, automations automationdom.Service) *Service {
	s.instanceID, s.runRepo, s.docs, s.annotations, s.automations = instance, repo, docs, annotations, automations
	return s
}

// RunAuthorization is the only server-to-server dispatch identity. Empty project
// and linkage strings are explicit; the boolean must always be present on wire.
type RunAuthorization struct {
	ConversationID string `json:"conversation_id"`
	AgentID        string `json:"agent_id"`
	InstanceID     string `json:"instance_id"`
	ProjectID      string `json:"project_id"`
	Audience       string `json:"audience"`
	OwnerUserID    string `json:"owner_user_id"`
	HasPriorTurns  bool   `json:"has_prior_turns"`
	Status         string `json:"status"`
	TaskID         string `json:"task_id"`
	CommentID      string `json:"comment_id"`
	ChatSessionID  string `json:"chat_session_id"`
}

func stringID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
func pointerID(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return stringID(*id)
}

func (s *Service) runOwner(ctx context.Context, c *agentdom.AgentConversation) (uuid.UUID, error) {
	if s.runRepo == nil || c.Audience != agentdom.AudienceOwnerPrivate {
		return uuid.Nil, agentdom.ErrConversationNotFound
	}
	var member, user uuid.UUID
	if c.ProjectID == uuid.Nil {
		if c.ActorUserID == nil || *c.ActorUserID == uuid.Nil {
			return uuid.Nil, agentdom.ErrConversationNotFound
		}
		user = *c.ActorUserID
	} else if c.ChatSessionID == nil || c.ActorUserID != nil {
		return uuid.Nil, agentdom.ErrConversationNotFound
	}
	if c.ChatSessionID != nil {
		session, err := s.repo.FindChatSessionByID(ctx, *c.ChatSessionID)
		if err != nil {
			return uuid.Nil, err
		}
		if session.ProjectID != c.ProjectID || session.AgentID != c.AgentID {
			return uuid.Nil, agentdom.ErrConversationNotFound
		}
		if c.ProjectID == uuid.Nil {
			if session.MemberID != uuid.Nil || session.ActorUserID == nil || *session.ActorUserID != user {
				return uuid.Nil, agentdom.ErrConversationNotFound
			}
		} else {
			if session.MemberID == uuid.Nil || session.ActorUserID != nil {
				return uuid.Nil, agentdom.ErrConversationNotFound
			}
			member = session.MemberID
		}
	}
	return s.runRepo.CurrentChatActor(ctx, c.ProjectID, member, user)
}

func (s *Service) chatAuthority(ctx context.Context, project, agent, member, user uuid.UUID) error {
	if s.runRepo == nil || user == uuid.Nil || s.authorizer == nil {
		return agentdom.ErrConversationNotFound
	}
	owner, err := s.runRepo.CurrentChatActor(ctx, project, member, user)
	if err != nil {
		return err
	}
	if owner != user {
		return agentdom.ErrConversationNotFound
	}
	if project == uuid.Nil {
		_, err = s.GetGlobalAgent(ctx, agent)
		return err
	}
	allowed, err := s.authorizer.HasPermissions(ctx, user, &project, "", authz.PermissionConversationsWrite)
	if err != nil {
		return err
	}
	if !allowed {
		return agentdom.ErrConversationNotFound
	}
	_, err = s.GetAgent(ctx, project, agent)
	return err
}

// validateRun covers dynamic descendants each time and also returns their
// snapshot dependencies for accumulation into the destination conversation.
func (s *Service) validateRun(ctx context.Context, c *agentdom.AgentConversation, owner uuid.UUID, visited map[uuid.UUID]bool) ([]agentdom.ContextItemRef, error) {
	actual, err := s.runOwner(ctx, c)
	if err != nil {
		return nil, err
	}
	if actual != owner || c.ContextItems == nil {
		return nil, agentdom.ErrConversationNotFound
	}
	if err = s.chatAuthority(ctx, c.ProjectID, c.AgentID, uuid.Nil, owner); err != nil {
		return nil, err
	}
	refs := append([]agentdom.ContextItemRef{}, c.ContextItems...)
	if c.TaskID != nil {
		p := c.ProjectID.String()
		refs = append(refs, agentdom.ContextItemRef{Type: agentdom.ContextItemTask, ID: c.TaskID.String(), ProjectID: &p})
	}
	if visited[c.ID] {
		return []agentdom.ContextItemRef{}, nil
	}
	visited[c.ID] = true
	return s.checkContext(ctx, c.ProjectID, owner, refs, visited)
}

func (s *Service) checkContext(ctx context.Context, project, owner uuid.UUID, refs []agentdom.ContextItemRef, visited map[uuid.UUID]bool) ([]agentdom.ContextItemRef, error) {
	out := []agentdom.ContextItemRef{}
	for _, ref := range refs {
		id, err := uuid.Parse(ref.ID)
		if err != nil || id == uuid.Nil {
			return nil, agentdom.ErrConversationNotFound
		}
		pid := project
		if ref.ProjectID != nil {
			pid, err = uuid.Parse(*ref.ProjectID)
			if err != nil || pid == uuid.Nil {
				return nil, agentdom.ErrConversationNotFound
			}
		}
		var title string
		switch ref.Type {
		case agentdom.ContextItemTask:
			if s.taskChecker == nil {
				return nil, agentdom.ErrConversationNotFound
			}
			obj, e := s.taskChecker.CheckTaskAccess(ctx, pid, id, taskdom.AccessRead)
			if e != nil {
				return nil, e
			}
			title = obj.Title
		case agentdom.ContextItemConversation:
			obj, e := s.repo.FindConversationByID(ctx, id)
			if e != nil {
				return nil, e
			}
			if obj.ProjectID != pid {
				return nil, agentdom.ErrConversationNotFound
			}
			nested, e := s.validateRun(ctx, obj, owner, visited)
			if e != nil {
				return nil, e
			}
			out = append(out, nested...)
			title = "Conversation"
		case agentdom.ContextItemDoc, agentdom.ContextItemAnnotation, agentdom.ContextItemAutomation:
			if s.authorizer == nil || s.runRepo == nil {
				return nil, agentdom.ErrConversationNotFound
			}
			permission := authz.PermissionDocsRead
			if ref.Type == agentdom.ContextItemAnnotation {
				permission = authz.PermissionAnnotationsRead
			}
			if ref.Type == agentdom.ContextItemAutomation {
				permission = authz.PermissionWorkflowsRead
			}
			if _, e := s.runRepo.CurrentChatActor(ctx, pid, uuid.Nil, owner); e != nil {
				return nil, e
			}
			allowed, e := s.authorizer.HasPermissions(ctx, owner, &pid, "", permission)
			if e != nil {
				return nil, e
			}
			if !allowed {
				return nil, agentdom.ErrConversationNotFound
			}
			switch ref.Type {
			case agentdom.ContextItemDoc:
				if s.docs == nil {
					return nil, agentdom.ErrConversationNotFound
				}
				obj, e := s.docs.GetDocument(ctx, id)
				if e != nil {
					return nil, e
				}
				if obj.ProjectID != pid {
					return nil, agentdom.ErrConversationNotFound
				}
				title = obj.Title
			case agentdom.ContextItemAnnotation:
				if s.annotations == nil {
					return nil, agentdom.ErrConversationNotFound
				}
				obj, e := s.annotations.Get(ctx, pid, id)
				if e != nil {
					return nil, e
				}
				title = "Annotation"
				if obj.TaskID != nil {
					nested, e := s.checkContext(ctx, pid, owner, []agentdom.ContextItemRef{{Type: agentdom.ContextItemTask, ID: obj.TaskID.String()}}, visited)
					if e != nil {
						return nil, e
					}
					out = append(out, nested...)
				}
			case agentdom.ContextItemAutomation:
				if s.automations == nil {
					return nil, agentdom.ErrConversationNotFound
				}
				_, e := s.automations.GetAutomation(ctx, pid, id)
				if e != nil {
					return nil, e
				}
				title = "Automation"
			}
		default:
			return nil, agentdom.ErrConversationNotFound
		}
		var p *string
		if pid != uuid.Nil {
			v := pid.String()
			p = &v
		}
		out = append(out, agentdom.ContextItemRef{Type: ref.Type, ID: id.String(), ProjectID: p, Title: title})
	}
	return out, nil
}

// AuthorizeRun is shared with future active-run/realtime reconciliation. It never
// uses queue actor/project attribution or confers permission to read old history.
func (s *Service) AuthorizeRun(ctx context.Context, conversation, agent uuid.UUID, begin bool) (*RunAuthorization, error) {
	if s.instanceID == uuid.Nil || s.runRepo == nil {
		return nil, agentdom.ErrConversationNotFound
	}
	c, err := s.repo.FindConversationByID(ctx, conversation)
	if err != nil {
		return nil, err
	}
	if c.AgentID != agent || (begin && c.Status != "running") {
		return nil, agentdom.ErrConversationNotFound
	}
	owner, err := s.runOwner(ctx, c)
	if err != nil {
		return nil, err
	}
	// Start from a fresh human context: queued service grants must not survive.
	humanCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	actorCtx := authz.WithActorID(humanCtx, owner)
	refs, err := s.validateRun(actorCtx, c, owner, map[uuid.UUID]bool{})
	if err != nil {
		return nil, err
	}
	if err = s.runRepo.MergeConversationContext(ctx, c.ID, refs); err != nil {
		return nil, err
	}
	prior, err := s.runRepo.BeginConversationTurn(ctx, c.ID, c.AgentID, begin)
	if err != nil {
		return nil, err
	}
	return &RunAuthorization{ConversationID: c.ID.String(), AgentID: c.AgentID.String(), InstanceID: s.instanceID.String(), ProjectID: stringID(c.ProjectID), Audience: string(c.Audience), OwnerUserID: owner.String(), HasPriorTurns: prior, Status: c.Status, TaskID: pointerID(c.TaskID), CommentID: pointerID(c.CommentID), ChatSessionID: pointerID(c.ChatSessionID)}, nil
}

func (s *Service) prepareChat(ctx context.Context, project, agent, member, user uuid.UUID, c *agentdom.AgentConversation, refs []agentdom.ContextItemRef) ([]agentdom.ContextItemRef, error) {
	if s.runRepo == nil && s.instanceID == uuid.Nil && len(refs) == 0 {
		return refs, nil
	}
	if err := agentdom.ValidateContextItems(refs); err != nil {
		return nil, err
	}
	actor, ok := authz.ActorIDFromContext(ctx)
	if !ok || actor != user || authz.IsAgentCredential(ctx) {
		return nil, agentdom.ErrConversationNotFound
	}
	if err := s.chatAuthority(ctx, project, agent, member, user); err != nil {
		return nil, err
	}
	checked, err := s.checkContext(ctx, project, user, refs, map[uuid.UUID]bool{})
	if err != nil {
		return nil, err
	}
	if c != nil && c.Audience != agentdom.AudienceOwnerPrivate && s.instanceID == uuid.Nil {
		if c.TaskID != nil {
			if s.taskChecker == nil {
				return nil, agentdom.ErrConversationNotFound
			}
			if _, err := s.taskChecker.CheckTaskAccess(ctx, c.ProjectID, *c.TaskID, taskdom.AccessRead); err != nil {
				return nil, err
			}
		}
		return checked, nil
	}
	if c != nil {
		old, e := s.validateRun(ctx, c, user, map[uuid.UUID]bool{})
		if e != nil {
			return nil, e
		}
		checked = append(checked, old...)
	}
	return checked, nil
}

// persistChatContext runs only after the continuation's busy/capacity/claim
// checks accept it, and before its trigger can expose the new context.
func (s *Service) persistChatContext(ctx context.Context, c *agentdom.AgentConversation, refs []agentdom.ContextItemRef) error {
	if s.runRepo == nil || c.Audience != agentdom.AudienceOwnerPrivate {
		return nil
	}
	err := s.runRepo.MergeConversationContext(ctx, c.ID, refs)
	if err == nil || c.Status == string(agentdom.ConversationStatusRunning) || c.Status == string(agentdom.ConversationStatusQueued) {
		return err
	}
	// c retains the pre-claim status on resume. Release only the new occupancy;
	// a concurrent stop or completion must survive a failed context merge.
	// Cleanup also needs to work when cancellation caused the merge failure.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, claimedStatus := range []agentdom.ConversationStatus{agentdom.ConversationStatusRunning, agentdom.ConversationStatusQueued} {
		restored, restoreErr := s.repo.ClaimConversationStatus(cleanupCtx, c.ID, string(claimedStatus), c.Status)
		if restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		if restored {
			break
		}
	}
	return err
}

func (s *Service) rejectSharedACP(ctx context.Context, agent uuid.UUID) error {
	if s.instanceID == uuid.Nil {
		return nil
	}
	a, err := s.repo.FindAgentByID(ctx, agent)
	if err != nil {
		return err
	}
	if a.AgentType == agentdom.AgentTypeACP {
		return agentdom.ErrConversationNotFound
	}
	return nil
}

func (s *Service) applyConversationPayload(c *agentdom.AgentConversation, p map[string]any) {
	if s.instanceID != uuid.Nil || c.Audience != agentdom.AudienceProjectShared {
		delete(p, "context_items")
	}
	for _, key := range []string{"actor_user_id", "actor_member_id", "owner_user_id", "audience", "instance_id"} {
		delete(p, key)
	}
	p["conversation_id"], p["agent_id"], p["project_id"], p["trigger_type"] = c.ID.String(), c.AgentID.String(), stringID(c.ProjectID), c.TriggerType
	p["task_id"], p["comment_id"], p["chat_session_id"] = pointerID(c.TaskID), pointerID(c.CommentID), pointerID(c.ChatSessionID)
	// Private refs come only from persistence. Explicit legacy shared runs retain
	// the API-validated per-turn payload without certifying unknown history.
}
func (s *Service) failQueuedRun(ctx context.Context, c *agentdom.AgentConversation) error {
	changed, err := s.repo.ClaimConversationStatus(ctx, c.ID, "queued", "failed")
	if err != nil {
		return err
	}
	if _, err := s.repo.DeletePendingTriggerByConversationID(ctx, c.ID); err != nil {
		return err
	}
	if changed && s.publisher != nil {
		return s.publisher.AppendFlat(ctx, "paca:agent:conversation_status", map[string]any{"conversation_id": c.ID.String(), "status": "failed"})
	}
	return nil
}

package agentsvc

import (
	"context"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/google/uuid"
)

// History checks current read access, never dispatch capability or run state.
// Explicit legacy deployments retain their existing shared-history semantics.
func (s *Service) authorizeHistory(ctx context.Context, c *agentdom.AgentConversation, user uuid.UUID) error {
	if s.instanceID == uuid.Nil {
		return nil
	}
	if s.runRepo == nil || user == uuid.Nil {
		return agentdom.ErrConversationNotFound
	}
	human, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// Drop queued/service/agent grants while preserving the caller's delivery lease.
	actor := authz.WithActorID(delivery.CopyLease(database.CopyTransaction(human, ctx), ctx), user)
	_, err := s.validateHistory(actor, c, user, map[uuid.UUID]bool{})
	if RunDenied(err) {
		return agentdom.ErrConversationNotFound
	}
	return err
}

func (s *Service) validateHistory(ctx context.Context, c *agentdom.AgentConversation, user uuid.UUID, visited map[uuid.UUID]bool) ([]agentdom.ContextItemRef, error) {
	if s.runRepo == nil || user == uuid.Nil || c.ContextItems == nil {
		return nil, agentdom.ErrConversationNotFound
	}
	var actual uuid.UUID
	var err error
	if c.Audience == agentdom.AudienceOwnerPrivate {
		actual, err = s.runOwner(ctx, c)
	} else {
		actual, err = s.runRepo.CurrentChatActor(ctx, c.ProjectID, uuid.Nil, user)
	}
	if err != nil {
		return nil, err
	}
	if actual != user {
		return nil, agentdom.ErrConversationNotFound
	}
	if visited[c.ID] {
		return []agentdom.ContextItemRef{}, nil
	}
	visited[c.ID] = true
	refs := append([]agentdom.ContextItemRef{}, c.ContextItems...)
	if c.TaskID != nil {
		p := c.ProjectID.String()
		refs = append(refs, agentdom.ContextItemRef{Type: agentdom.ContextItemTask, ID: c.TaskID.String(), ProjectID: &p})
	}
	return s.checkContext(ctx, c.ProjectID, user, refs, visited)
}

func (s *Service) authorizeAgentHistory(ctx context.Context, current, target *agentdom.AgentConversation) error {
	if s.instanceID == uuid.Nil {
		return nil
	}
	user, err := s.runOwner(ctx, current)
	if err != nil {
		return agentdom.ErrConversationNotFound
	}
	if err = s.authorizeHistory(ctx, current, user); err != nil {
		return err
	}
	if current.ID == target.ID {
		return nil
	}
	return s.authorizeHistory(ctx, target, user)
}

func (s *Service) listReadableConversations(ctx context.Context, in agentdom.ListConversationsFilter, limit int) ([]*agentdom.AgentConversation, bool, error) {
	if s.instanceID == uuid.Nil {
		return s.repo.ListConversations(ctx, in, limit)
	}
	user, _ := authz.ActorIDFromContext(ctx)
	out := []*agentdom.AgentConversation{}
	// Scan existing keyset windows until there is one extra authorized row. Hidden
	// sources must neither consume pages nor leak existence through search cursors.
	for {
		rows, more, err := s.repo.ListConversations(ctx, in, limit)
		if err != nil {
			return nil, false, err
		}
		for _, c := range rows {
			if err := s.authorizeHistory(ctx, c, user); err != nil {
				if RunDenied(err) {
					continue
				}
				return nil, false, err
			}
			out = append(out, c)
			if len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if !more || len(rows) == 0 {
			return out, false, nil
		}
		cursor := agentdom.EncodeConversationCursor(rows[len(rows)-1])
		in.CursorAfter = &cursor
	}
}

func (s *Service) readableChatSessions(ctx context.Context, sessions []*agentdom.AgentChatSession) ([]*agentdom.AgentChatSession, error) {
	if s.instanceID == uuid.Nil {
		return sessions, nil
	}
	user, _ := authz.ActorIDFromContext(ctx)
	out := []*agentdom.AgentChatSession{}
	for _, session := range sessions {
		filter := agentdom.ListConversationsFilter{ChatSessionID: &session.ID}
		readable := true
		for readable {
			rows, more, err := s.repo.ListConversations(ctx, filter, 100)
			if err != nil {
				return nil, err
			}
			for _, c := range rows {
				if err = s.authorizeHistory(ctx, c, user); err != nil {
					if !RunDenied(err) {
						return nil, err
					}
					readable = false
					break
				}
			}
			if !more || len(rows) == 0 {
				break
			}
			cursor := agentdom.EncodeConversationCursor(rows[len(rows)-1])
			filter.CursorAfter = &cursor
		}
		if readable {
			out = append(out, session)
		}
	}
	return out, nil
}

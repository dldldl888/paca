package handler

import (
	"context"
	"errors"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/google/uuid"
	"time"
)

func (h *Handler) protect(ctx context.Context, t agent.Trigger) (context.Context, func(), error) {
	if h.RunAuthorizer == nil {
		return ctx, func() {}, nil
	}
	ctx, epoch, leave, err := h.RunAuthorizer.Enter(ctx)
	if err != nil {
		return ctx, func() {}, err
	}
	a, err := h.RunAuthorizer.Authorize(ctx, t.ConversationID, t.AgentID, false)
	if err == nil && (a.Status != "running" || (epoch != "" && a.AuthorizationEpoch != epoch)) {
		err = runauth.ErrDenied
	}
	if err != nil {
		leave()
		return ctx, func() {}, err
	}
	return ctx, leave, nil
}
func (h *Handler) ReconcileAuthorization(ctx context.Context) error {
	ids, err := h.ConvRepo.RevokedRuns(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if h.InFlight != nil {
			if err = h.InFlight.InterruptAndWait(ctx, id); err != nil {
				return err
			}
		}
		if err = h.Publisher.PublishConversationStatus(ctx, id, "failed"); err != nil {
			return err
		}
		project, owner, err := h.ConvRepo.GetConversationRealtimeContext(ctx, id)
		if err != nil {
			return err
		}
		if err = h.Publisher.PublishRealtime(ctx, project, id, "agent.authorization_revoked", nil, owner); err != nil {
			return err
		}
	}
	return nil
}
func (h *Handler) currentTerminal(ctx context.Context, id uuid.UUID, status string) bool {
	current, err := h.ConvRepo.GetStatus(ctx, id)
	return err == nil && current == status
}
func waitAuthorization(ctx context.Context, fn func() (context.Context, func(), error)) (context.Context, func(), error) {
	for {
		next, leave, err := fn()
		if !errors.Is(err, runauth.ErrBlocked) {
			return next, leave, err
		}
		select {
		case <-ctx.Done():
			return ctx, func() {}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

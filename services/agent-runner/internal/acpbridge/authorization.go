package acpbridge

import (
	"context"
	"errors"
	"fmt"
	"github.com/Paca-AI/agent-runner/internal/repository/postgres"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/google/uuid"
	"strconv"
	"time"
)

func (r *Registry) WithAuthorization(client *runauth.Client, repo *postgres.ConversationRepository) *Registry {
	r.authorizer = client
	r.convRepo = repo
	return r
}

// Native boot registrations survive lost sockets. Only acknowledged native shutdown removes one.
func (r *Registry) EnrollNative(ctx context.Context, agentID uuid.UUID, session, boot string) error {
	if r.authorizer == nil {
		return nil
	}
	if id, err := uuid.Parse(boot); err != nil || id == uuid.Nil {
		return fmt.Errorf("missing native boot id")
	}
	r.mu.Lock()
	entry := r.connections[agentID]
	if entry == nil || entry.sessionID != session {
		r.mu.Unlock()
		return runauth.ErrUnavailable
	}
	entry.bootID = boot
	phase, epoch := r.deliveryPhase, r.deliveryEpoch
	r.mu.Unlock()
	// Existing disconnected participants must be able to ACK an already pending CLOSE.
	if phase != "" && phase != "open" {
		if err := r.sendBarrier(ctx, agentID, entry, phase, epoch); err != nil {
			return err
		}
	}
	state, err := r.authorizer.Enroll(ctx, boot)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.connections[agentID] != entry {
		r.mu.Unlock()
		return runauth.ErrUnavailable
	}
	entry.enrolled = true
	phase, epoch = r.deliveryPhase, r.deliveryEpoch
	if phase == "" {
		epoch = state.Epoch
		phase = "close"
		if !state.Blocked {
			phase = "open"
		}
	}
	r.mu.Unlock()
	return r.sendBarrier(ctx, agentID, entry, phase, epoch)
}
func (r *Registry) sendBarrier(ctx context.Context, agentID uuid.UUID, entry *connEntry, phase, epoch string) error {
	r.mu.Lock()
	fenced := entry.fenced
	r.mu.Unlock()
	if fenced {
		return r.NativeAck(ctx, agentID, entry.sessionID, phase, epoch)
	}
	message := map[string]any{"type": "authorization_barrier", "phase": phase, "authorization_epoch": epoch}
	if phase == "reconcile" {
		allowed, err := r.convRepo.AuthorizedRunningIDs(ctx, agentID)
		if err != nil {
			return err
		}
		message["allowed_conversation_ids"] = allowed
	}
	return entry.conn.SendJSON(ctx, message)
}
func (r *Registry) AuthorizationControl(ctx context.Context, phase, epoch string) error {
	r.mu.Lock()
	r.deliveryPhase, r.deliveryEpoch = phase, epoch
	entries := make(map[uuid.UUID]*connEntry, len(r.connections))
	for id, e := range r.connections {
		if phase != "open" || e.enrolled {
			entries[id] = e
		}
	}
	r.mu.Unlock()
	for id, e := range entries {
		if err := r.sendBarrier(ctx, id, e, phase, epoch); err != nil {
			return err
		}
	}
	return nil
}
func (r *Registry) NativeAck(ctx context.Context, agentID uuid.UUID, session, phase, epoch string) error {
	if r.authorizer == nil {
		return nil
	}
	r.mu.Lock()
	entry := r.connections[agentID]
	boot := ""
	if entry != nil && entry.sessionID == session {
		boot = entry.bootID
	}
	r.mu.Unlock()
	if boot == "" {
		return runauth.ErrUnavailable
	}
	return r.authorizer.Ack(ctx, r.redis, boot, phase, epoch)
}
func (r *Registry) NativeShutdown(ctx context.Context, agentID uuid.UUID, session string) error {
	if r.authorizer == nil {
		return nil
	}
	r.mu.Lock()
	entry := r.connections[agentID]
	boot := ""
	if entry != nil && entry.sessionID == session {
		boot = entry.bootID
		entry.fenced = true
	}
	phase, epoch := r.deliveryPhase, r.deliveryEpoch
	r.mu.Unlock()
	if boot == "" {
		return runauth.ErrUnavailable
	}
	if phase == "close" || phase == "reconcile" {
		if err := r.NativeAck(ctx, agentID, entry.sessionID, phase, epoch); err != nil {
			return err
		}
	}
	return r.authorizer.Unenroll(ctx, boot)
}

func (s *Server) protectMessage(ctx context.Context, agentID uuid.UUID, msg map[string]any) (context.Context, func(), error) {
	for {
		next, leave, err := s.tryProtectMessage(ctx, agentID, msg)
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
func (s *Server) tryProtectMessage(ctx context.Context, agentID uuid.UUID, msg map[string]any) (context.Context, func(), error) {
	if s.RunAuthorizer == nil {
		return ctx, func() {}, nil
	}
	ctx, epoch, leave, err := s.RunAuthorizer.Enter(ctx)
	if err != nil {
		return ctx, func() {}, err
	}
	cid, e := uuid.Parse(fmt.Sprint(msg["conversation_id"]))
	if e != nil {
		leave()
		return ctx, func() {}, runauth.ErrDenied
	}
	a, err := s.RunAuthorizer.Authorize(ctx, cid, agentID, false)
	wire, ok := msg["authorization_epoch"].(string)
	wireEpoch, wireErr := strconv.ParseUint(wire, 10, 63)
	current, _ := strconv.ParseUint(epoch, 10, 63)
	// Old buffered output from an unaffected running turn is authorized again
	// under the current epoch. Revoked runs are already terminal in the API.
	if err == nil && (a.Status != "running" || a.AuthorizationEpoch != epoch || !ok || wireErr != nil || strconv.FormatUint(wireEpoch, 10) != wire || wireEpoch > current) {
		err = runauth.ErrDenied
	}
	if err != nil {
		leave()
		return ctx, func() {}, err
	}
	return ctx, leave, nil
}

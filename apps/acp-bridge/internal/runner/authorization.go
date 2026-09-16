package runner

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Paca-AI/paca/apps/acp-bridge/internal/acpclient"
)

// AuthorizationBarrier runs on the control path, independently of queued output.
func (r *Runner) AuthorizationBarrier(ctx context.Context, phase, epoch string, allowed []string) error {
	n, err := strconv.ParseUint(epoch, 10, 63)
	if err != nil || strconv.FormatUint(n, 10) != epoch {
		return errors.New("invalid authorization epoch")
	}
	r.requestMu.Lock()
	previous, _ := strconv.ParseUint(r.authorizationEpoch, 10, 63)
	if n < previous {
		r.requestMu.Unlock()
		return errors.New("stale authorization barrier")
	}
	r.authorizationEpoch = epoch
	if phase == "open" {
		r.authorizationBlocked = false
		r.requestMu.Unlock()
		return nil
	}
	if phase != "close" && phase != "reconcile" {
		r.requestMu.Unlock()
		return errors.New("invalid barrier phase")
	}
	r.authorizationBlocked = true
	r.requestMu.Unlock()
	keep := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		keep[id] = true
	}
	var clients []*acpclient.Client
	var turns []chan struct{}
	r.mu.Lock()
	for _, s := range r.conversations {
		s.mu.Lock()
		stop := phase == "reconcile" && !keep[s.currentConversationID]
		if stop {
			if s.turnCancel != nil {
				s.turnCancel()
			}
			if s.client != nil {
				clients = append(clients, s.client)
				s.client = nil
				s.sessionID = ""
			}
			if s.turnRunning && s.turnDone != nil {
				turns = append(turns, s.turnDone)
			}
			s.generation++
			s.updatesEnabled = false
		} else if phase == "reconcile" {
			s.authorizationEpoch = epoch
		}
		s.mu.Unlock()
	}
	r.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
	for _, done := range turns {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (r *Runner) requestFence(ctx context.Context, state *conversationState, method string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.requestMu.RLock()
		if !r.authorizationBlocked {
			state.mu.Lock()
			allowed := state.authorizationEpoch == r.authorizationEpoch
			if allowed && method == "session/prompt" {
				state.promptStarted = true
			}
			state.mu.Unlock()
			if !allowed {
				r.requestMu.RUnlock()
				return nil, context.Canceled
			}
			return r.requestMu.RUnlock, nil
		}
		r.requestMu.RUnlock()
		// CLOSE pauses lawful setup; RECONCILE cancels revoked turns before OPEN.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (r *Runner) AuthorizationDisconnected() {
	r.requestMu.Lock()
	r.authorizationBlocked = true
	r.requestMu.Unlock()
}

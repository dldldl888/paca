// Package delivery fences short protected side effects, not whole model turns.
package delivery

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

var ErrBlocked = errors.New("authorization transition pending")

type State struct {
	Epoch   string `json:"epoch"`
	Blocked bool   `json:"blocked"`
}
type Gate struct {
	state atomic.Pointer[State]
	work  sync.RWMutex
}
type leaseKey struct{}

func NewGate() *Gate         { g := &Gate{}; g.state.Store(&State{Epoch: "0", Blocked: true}); return g }
func (g *Gate) State() State { return *g.state.Load() }
func (g *Gate) Update(epoch string, blocked bool) error {
	n, err := strconv.ParseUint(epoch, 10, 63)
	if err != nil || strconv.FormatUint(n, 10) != epoch {
		return errors.New("invalid authorization epoch")
	}
	for {
		old := g.state.Load()
		previous, _ := strconv.ParseUint(old.Epoch, 10, 63)
		if n < previous {
			return nil
		}
		if g.state.CompareAndSwap(old, &State{Epoch: epoch, Blocked: blocked}) {
			return nil
		}
	}
}
func (g *Gate) Close(epoch string) error {
	if err := g.Update(epoch, true); err != nil {
		return err
	}
	g.work.Lock()
	g.work.Unlock()
	return nil
}
func (g *Gate) Disconnect() { s := g.State(); _ = g.Close(s.Epoch) }
func (g *Gate) Enter(ctx context.Context) (context.Context, string, func(), error) {
	if epoch, ok := ctx.Value(leaseKey{}).(string); ok {
		return ctx, epoch, func() {}, nil
	}
	g.work.RLock()
	s := g.State()
	if s.Blocked {
		g.work.RUnlock()
		return ctx, "", func() {}, ErrBlocked
	}
	return context.WithValue(ctx, leaseKey{}, s.Epoch), s.Epoch, g.work.RUnlock, nil
}

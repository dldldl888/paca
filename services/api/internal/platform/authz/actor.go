package authz

import (
	"context"
	"github.com/google/uuid"
)

type actorContextKey struct{}
type agentContextKey struct{}

func WithActorID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, actorContextKey{}, id)
}
func ActorIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(actorContextKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}
func WithAgentID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, agentContextKey{}, id)
}
func AgentIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(agentContextKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

// WithAgentCredential marks a shared or per-agent credential independently of
// optional identity headers. A missing agent ID must not turn its bot into a user.
func WithAgentCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentCredentialKey{}, true)
}

type agentCredentialKey struct{}

func IsAgentCredential(ctx context.Context) bool {
	value, _ := ctx.Value(agentCredentialKey{}).(bool)
	return value
}

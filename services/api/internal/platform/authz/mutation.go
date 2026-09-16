package authz

import "context"

type mutationChecksKey struct{}

// WithMutationCheck retains required actions, never the earlier allow result.
// The repository coordinator reruns each check in the authority transaction.
func WithMutationCheck(ctx context.Context, check func(context.Context) error) context.Context {
	previous, _ := ctx.Value(mutationChecksKey{}).([]func(context.Context) error)
	checks := append([]func(context.Context) error{}, previous...)
	checks = append(checks, check)
	return context.WithValue(ctx, mutationChecksKey{}, checks)
}
func RecheckMutation(ctx context.Context) error {
	checks, _ := ctx.Value(mutationChecksKey{}).([]func(context.Context) error)
	for _, check := range checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}

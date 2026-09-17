package bootstrap

import (
	"context"
	"errors"
	"net/http"
)

// The workers own independent read loops. Drain them concurrently with HTTP
// while retaining delivery and publishing until all accepted work finishes.
func shutdown(ctx context.Context, server *http.Server, stops []func() error, deliveryStop func(context.Context) error, closePublisher func()) error {
	drained := make(chan error, len(stops)+1)
	go func() { drained <- server.Shutdown(ctx) }()
	for _, stop := range stops {
		go func() { drained <- stop() }()
	}
	var drainErr error
	for range len(stops) + 1 {
		select {
		case err := <-drained:
			drainErr = errors.Join(drainErr, err)
		case <-ctx.Done():
			return errors.Join(drainErr, ctx.Err())
		}
	}
	if drainErr != nil {
		return drainErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deliveryStop != nil {
		// Gate disconnection can wait on an in-flight lease; the caller's
		// budget still applies. An expired ctx also prevents late Unenroll.
		stopped := make(chan error, 1)
		go func() { stopped <- deliveryStop(ctx) }()
		select {
		case err := <-stopped:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if closePublisher != nil {
		closePublisher()
	}
	return nil
}

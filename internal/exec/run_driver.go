package exec

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type runDriverKey struct{}

func (s *Supervisor) keepRunDriver(parent context.Context, lease ledger.Lease, ttl time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.ledger.Renew(ctx, lease, ttl); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return context.WithValue(ctx, runDriverKey{}, lease), func() { cancel(context.Canceled); <-done; _ = s.ledger.Release(context.WithoutCancel(parent), lease) }
}
func runFence(ctx context.Context) []ledger.Lease {
	if lease, ok := ctx.Value(runDriverKey{}).(ledger.Lease); ok {
		return []ledger.Lease{lease}
	}
	return nil
}

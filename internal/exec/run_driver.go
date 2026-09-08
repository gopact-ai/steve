package exec

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type runDriverKey struct{}

// renewTicks paces a driver's lease renewals; the default is a real ticker
// at a third of the TTL, tests hand over a channel they drive themselves.
func renewTicks(every time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(every)
	return ticker.C, ticker.Stop
}

func (s *Supervisor) keepRunDriver(parent context.Context, lease ledger.Lease, ttl time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	ticks := s.driverTicks
	if ticks == nil {
		ticks = renewTicks
	}
	go func() {
		defer close(done)
		tick, stop := ticks(ttl / 3)
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				if _, err := s.ledger.Renew(ctx, lease, ttl); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return context.WithValue(ctx, runDriverKey{}, lease), func() {
		cancel(context.Canceled)
		<-done
		// The driver lease only fences this run; once its goroutine has
		// stopped, a release that fails (already expired or invalidated)
		// leaves nothing to reclaim.
		_ = s.ledger.Release(context.WithoutCancel(parent), lease)
	}
}
func runFence(ctx context.Context) []ledger.Lease {
	if lease, ok := ctx.Value(runDriverKey{}).(ledger.Lease); ok {
		return []ledger.Lease{lease}
	}
	return nil
}

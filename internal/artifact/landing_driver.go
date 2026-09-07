package artifact

import (
	"context"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type landingDriverKey struct{}
type landingDriver struct {
	lease     ledger.Lease
	ctx       context.Context
	mu        sync.Mutex
	canonical *ledger.Lease
	done      chan struct{}
}

func (s *Store) startLandingDriver(parent context.Context, lease ledger.Lease, ttl time.Duration) (context.Context, func()) {
	lifetime, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	d := &landingDriver{lease: lease, ctx: lifetime, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-ticker.C:
				if _, err := s.ledger.Renew(lifetime, lease, ttl); err != nil {
					cancel(err)
					return
				}
				d.mu.Lock()
				if d.canonical != nil {
					if _, err := s.ledger.RenewAny(lifetime, *d.canonical, landTTL); err != nil {
						cancel(err)
					}
				}
				d.mu.Unlock()
			}
		}
	}()
	ctx, stopWork := context.WithCancelCause(parent)
	stopWatch := context.AfterFunc(lifetime, func() { stopWork(context.Cause(lifetime)) })
	ctx = context.WithValue(ctx, landingDriverKey{}, d)
	return ctx, func() {
		cancel(context.Canceled)
		<-d.done
		stopWatch()
		stopWork(context.Canceled)
		_ = s.ledger.Release(context.WithoutCancel(parent), lease)
	}
}

func landingFence(ctx context.Context, leases []ledger.Lease) []ledger.Lease {
	if d, ok := ctx.Value(landingDriverKey{}).(*landingDriver); ok {
		return append(leases, d.lease)
	}
	return leases
}
func landingHolder(ctx context.Context, fallback string) string {
	if d, ok := ctx.Value(landingDriverKey{}).(*landingDriver); ok {
		return d.lease.Holder
	}
	return fallback
}
func trackLandingLease(ctx context.Context, lease ledger.Lease) func() {
	if d, ok := ctx.Value(landingDriverKey{}).(*landingDriver); ok {
		d.mu.Lock()
		d.canonical = &lease
		d.mu.Unlock()
		return func() { d.mu.Lock(); d.canonical = nil; d.mu.Unlock() }
	}
	return func() {}
}

// A user stop cannot abandon an admitted WAL, but driver loss must stop its
// writes before a replacement resumes the WAL under a new canonical lease.
func landingApplyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	apply, cancel := context.WithTimeout(context.WithoutCancel(ctx), landTTL)
	if d, ok := ctx.Value(landingDriverKey{}).(*landingDriver); ok {
		stop := context.AfterFunc(d.ctx, cancel)
		return apply, func() { stop(); cancel() }
	}
	return apply, cancel
}

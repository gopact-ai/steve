package artifact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// renewTicks paces a driver's lease renewals; the default is a real ticker
// at a third of the TTL, tests hand over a channel they drive themselves.
func renewTicks(every time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(every)
	return ticker.C, ticker.Stop
}

func (s *Store) startLandingDriver(parent context.Context, lease ledger.Lease, ttl time.Duration) (context.Context, func()) {
	lifetime, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	d := &landingDriver{lease: lease, ctx: lifetime, done: make(chan struct{})}
	ticks := s.renewTicks
	if ticks == nil {
		ticks = renewTicks
	}
	go func() {
		defer close(d.done)
		tick, stop := ticks(ttl / 3)
		defer stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-tick:
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
		// The driver lease only fences this landing; once its goroutine has
		// stopped, a release that fails (already expired or invalidated)
		// leaves nothing to reclaim.
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

// keepCanonical keeps a canonical lock the landing took alive for as long
// as the landing works: the landing driver renews it alongside its own
// lease when there is one, and a landing without a driver — a direct
// Land, a recovery run by boot or the periodic retry — renews it itself.
// A lock found stale stops renewing, and the next fenced transition is
// what refuses it; any other failure is retried on the next tick. The returned func stops renewing and waits.
func (s *Store) keepCanonical(ctx context.Context, lease ledger.Lease) func() {
	if d, ok := ctx.Value(landingDriverKey{}).(*landingDriver); ok {
		d.mu.Lock()
		d.canonical = &lease
		d.mu.Unlock()
		return func() { d.mu.Lock(); d.canonical = nil; d.mu.Unlock() }
	}
	ticks := s.renewTicks
	if ticks == nil {
		ticks = renewTicks
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick, stop := ticks(landTTL / 3)
		defer stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-tick:
				if _, err := s.ledger.RenewAny(lifetime, lease, landTTL); err != nil {
					if lifetime.Err() != nil {
						return
					}
					if errors.Is(err, ledger.ErrStale) {
						slog.Warn(fmt.Sprintf("artifact: canonical lock %s lost while landing: %v", lease.Key, err), "lease", lease.Key, "holder", lease.Holder)
						return
					}
					// A busy database or an unreachable issuer may pass
					// before the lease runs out; the next tick tries again.
					slog.Warn(fmt.Sprintf("artifact: renew canonical lock %s: %v", lease.Key, err), "lease", lease.Key, "holder", lease.Holder)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
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

// applying marks a landing as being applied by this process until the
// returned func is called. Its lock may be lost while its apply still
// writes — a lender let go, a renewal failed — and only this process
// knows the writer is still there (see RetryRecoveries).
func (s *Store) applying(id string) func() {
	token := new(int)
	s.inApply.Store(id, token)
	return func() { s.inApply.CompareAndDelete(id, token) }
}

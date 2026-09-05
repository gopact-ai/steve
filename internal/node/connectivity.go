package node

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/idle"
)

type idleHook struct{ clock idle.Clock }

// RegisterIdle pauses the actual running attempt's silence clock. The hook
// belongs to that attempt, not to its shared harness process. Cancellation
// and the parent's MaxElapsed deadline remain outside this clock.
func (r *Registry) RegisterIdle(name string, clock idle.Clock) func() {
	r.mu.Lock()
	if _, ok := r.confs[name]; !ok {
		r.mu.Unlock()
		return func() {}
	}
	if r.idleHooks[name] == nil {
		r.idleHooks[name] = map[*idleHook]struct{}{}
	}
	hook := &idleHook{clock: clock}
	r.idleHooks[name][hook] = struct{}{}
	if c := r.live[name]; c == nil || !c.alive() {
		clock.Pause()
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.idleHooks[name], hook)
		r.mu.Unlock()
	}
}

func (r *Registry) clocksLocked(name string, up bool) {
	for hook := range r.idleHooks[name] {
		if up {
			hook.clock.Resume()
		} else {
			hook.clock.Pause()
		}
	}
}

func (r *Registry) signalLocked(name string) {
	if ch := r.changed[name]; ch != nil {
		close(ch)
	}
	r.changed[name] = make(chan struct{})
}

// down rejects an old socket's late notification before it can change either
// the roster, an idle clock, or connectivity history. eventMu also orders
// observer callbacks, which are invoked without holding the registry lock.
func (r *Registry) down(c *conn) {
	r.eventMu.Lock()
	defer r.eventMu.Unlock()
	r.mu.Lock()
	if r.live[c.name] != c || c.released.Load() {
		r.mu.Unlock()
		return
	}
	delete(r.live, c.name)
	cfg := r.confs[c.name]
	down := Status{Name: c.name, Addr: cfg.Addr, Generation: c.generation, Level: levelOr(cfg.Level), Region: cfg.Region, LastError: "disconnected", Advert: c.getAdvert()}
	r.last[c.name] = &down
	r.signalLocked(c.name)
	r.clocksLocked(c.name, false)
	observe := r.observe
	r.mu.Unlock()
	log.Printf("node: %s disconnected (connection %d)", c.name, c.generation)
	if observe != nil {
		observe(down)
	}
}

// awaitConnection is driven by registry changes. The retry is also useful
// for callers that use Transport without the fleet's background Start loop.
func (r *Registry) awaitConnection(ctx context.Context, name string) (*conn, error) {
	timer := time.NewTimer(RedialEvery)
	defer timer.Stop()
	for {
		r.mu.Lock()
		_, known := r.confs[name]
		if !known || r.closed {
			r.mu.Unlock()
			return nil, fmt.Errorf("node %q released", name)
		}
		if c := r.live[name]; c != nil && c.alive() {
			r.mu.Unlock()
			return c, nil
		}
		if r.changed[name] == nil {
			r.changed[name] = make(chan struct{})
		}
		changed := r.changed[name]
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-timer.C:
			if c, err := r.connect(ctx, name); err == nil {
				return c, nil
			}
			timer.Reset(RedialEvery)
		}
	}
}

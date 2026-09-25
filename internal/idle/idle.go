// Package idle is a context that ends after silence, not after a fixed
// total. A turn that coordinates other agents, or a delegated child
// that builds and tests, can honestly take longer than any prompt
// would, but it never goes quiet for long: every tool call and every
// chunk of text is a sign of life. What the timeout catches is an
// agent that hung, not one that is busy.
package idle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errSilent is the cause of a clock that ran out.
var errSilent = fmt.Errorf("%w: silent past the idle timeout", context.DeadlineExceeded)

// Expired reports whether ctx ended because the silence clock it derives
// from ran out, not because its parent ended, the clock was stopped or ctx
// itself was cancelled first.
func Expired(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errSilent) }

// idleContext is the context; its error on expiry is
// context.DeadlineExceeded, like a deadline's. It wraps a cancel-cause
// context that ends with it, so context.Cause of it and of every context
// derived from it names why it ended.
type idleContext struct {
	context.Context
	end       context.CancelCauseFunc
	done      chan struct{}
	mu        sync.Mutex
	err       error
	timer     *time.Timer
	d         time.Duration
	remaining time.Duration
	due       time.Time
	paused    bool
	holds     int
	epoch     uint64
}

type clockKey struct{}

// Value lets Hold find the clock through any context derived from it.
func (c *idleContext) Value(key any) any {
	if key == (clockKey{}) {
		return c
	}
	return c.Context.Value(key)
}

// Hold stops the silence clock ctx derives from while the caller waits on
// something that is not the agent — a person answering a question. Holds
// nest and are independent of Pause: a connection coming back does not
// restart a clock a pending question still holds. Every release counts as
// a sign of life: the clock runs a whole silence again once nothing holds it. Without a clock in ctx, Hold does nothing.
func Hold(ctx context.Context) (release func()) {
	c, _ := ctx.Value(clockKey{}).(*idleContext)
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.err == nil {
		c.stopLocked()
	}
	c.holds++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.holds--
			c.remaining = c.d
			if c.err == nil && c.holds == 0 && !c.paused {
				c.arm(c.remaining)
			}
		})
	}
}

// Detach keeps ctx's values but not its silence clock. Work that outlives
// the context it came from must not hold a clock it no longer runs under.
func Detach(ctx context.Context) context.Context { return unclocked{ctx} }

type unclocked struct{ context.Context }

func (u unclocked) Value(key any) any {
	if key == (clockKey{}) {
		return nil
	}
	return u.Context.Value(key)
}

// Clock can suspend silence accounting without suspending a parent's hard
// deadline. Registrar is the wiring seam; users need not import node.
type Clock interface {
	Pause()
	Resume()
}
type Registrar func(node string, clock Clock) (unregister func())

type Context interface {
	context.Context
	Clock
}

// WithTimeout wraps parent; touch resets the clock, stop ends it.
func WithTimeout(parent context.Context, d time.Duration) (ctx Context, stop func(), touch func()) {
	inner, end := context.WithCancelCause(parent)
	c := &idleContext{Context: inner, end: end, done: make(chan struct{}), d: d}
	c.mu.Lock()
	c.arm(d)
	c.mu.Unlock()
	go func() {
		select {
		case <-parent.Done():
			c.finish(parent.Err())
		case <-c.done:
		}
	}()
	return c, func() { c.finish(context.Canceled) }, c.touch
}

func (c *idleContext) Done() <-chan struct{} { return c.done }

func (c *idleContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Deadline is the parent's: the idle clock is not a point in time.
func (c *idleContext) Deadline() (time.Time, bool) { return c.Context.Deadline() }

func (c *idleContext) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		if c.stopped() {
			c.remaining = c.d
		} else {
			c.arm(c.d)
		}
	}
}

// arm invalidates callbacks already waiting for the lock. Timer.Stop alone
// cannot keep an old expiry callback from racing with Pause or touch.
func (c *idleContext) arm(d time.Duration) {
	c.epoch++
	epoch := c.epoch
	if c.timer != nil {
		c.timer.Stop()
	}
	c.due = time.Now().Add(d)
	c.timer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err == nil && !c.stopped() && c.epoch == epoch {
			c.finishLocked(context.DeadlineExceeded, errSilent)
		}
	})
}

func (c *idleContext) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.paused {
		return
	}
	c.stopLocked()
	c.paused = true
}

func (c *idleContext) stopped() bool { return c.paused || c.holds > 0 }

// stopLocked saves what is left of the silence the first time the clock
// stops; a clock already stopped keeps what it saved.
func (c *idleContext) stopLocked() {
	if c.stopped() {
		return
	}
	c.remaining = max(0, time.Until(c.due))
	c.epoch++
	c.timer.Stop()
}

func (c *idleContext) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || !c.paused {
		return
	}
	c.paused = false
	if c.holds == 0 {
		c.arm(c.remaining)
	}
}

func (c *idleContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(err, nil)
}

// finishLocked records the cause before done closes: a context derived
// from this one reads it when it sees done.
func (c *idleContext) finishLocked(err, cause error) {
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	c.end(cause)
	close(c.done)
}

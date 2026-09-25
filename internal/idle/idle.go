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
	"sync/atomic"
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
//
// A context derived from it directly — by context.WithCancel, WithTimeout
// and the like — ends while it ends, before its Done closes: whoever sees
// it ended and then reads one derived from it reads that one ended too,
// for the same reason. One derived through a context that is not the
// context package's own, such as one from context.WithValue, hears later.
type idleContext struct {
	context.Context
	end  context.CancelCauseFunc
	done chan struct{}
	// err is set once, under mu, before the derived contexts are ended; it
	// is read without mu because ending them reads it.
	err       atomic.Value
	mu        sync.Mutex
	after     map[*func()]struct{}
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
	if c.Err() == nil {
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
			if c.Err() == nil && c.holds == 0 && !c.paused {
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
			c.finish(parent.Err(), context.Cause(parent))
		case <-c.done:
		}
	}()
	return c, func() { c.finish(context.Canceled, nil) }, c.touch
}

func (c *idleContext) Done() <-chan struct{} { return c.done }

// Err may report the end while the contexts derived from c are being ended,
// just before Done closes.
func (c *idleContext) Err() error {
	err, _ := c.err.Load().(error)
	return err
}

// AfterFunc makes c a parent the context package cancels its children
// through: it registers f, which ends one derived context, to run when c
// ends, before Done closes. f runs holding c.mu, so it must not call back
// into c except for Err, Value and Deadline; the context package's never
// does. On a c that has already ended, f runs at once in its own goroutine:
// the caller may hold the lock f takes.
func (c *idleContext) AfterFunc(f func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err() != nil {
		go f()
		return func() bool { return false }
	}
	key := &f
	if c.after == nil {
		c.after = map[*func()]struct{}{}
	}
	c.after[key] = struct{}{}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, registered := c.after[key]
		delete(c.after, key)
		return registered
	}
}

// Deadline is the parent's: the idle clock is not a point in time.
func (c *idleContext) Deadline() (time.Time, bool) { return c.Context.Deadline() }

func (c *idleContext) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err() == nil {
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
		if c.Err() == nil && !c.stopped() && c.epoch == epoch {
			c.finishLocked(context.DeadlineExceeded, errSilent)
		}
	})
}

func (c *idleContext) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err() != nil || c.paused {
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
	if c.Err() != nil || !c.paused {
		return
	}
	c.paused = false
	if c.holds == 0 {
		c.arm(c.remaining)
	}
}

func (c *idleContext) finish(err, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(err, cause)
}

// finishLocked ends c on err and cause, unless its parent ended first: then
// c ends for the parent's reason, whatever it was about to end on. The
// cause is recorded, and every context derived from c ended with it,
// before done closes.
func (c *idleContext) finishLocked(err, cause error) {
	if c.Err() != nil {
		return
	}
	if cause == nil {
		cause = err
	}
	c.end(cause)
	if !errors.Is(context.Cause(c.Context), cause) {
		err = c.Context.Err()
	}
	c.err.Store(err)
	c.timer.Stop()
	for f := range c.after {
		(*f)()
	}
	c.after = nil
	close(c.done)
}

// Package idle is a context that ends after silence, not after a fixed
// total. A turn that coordinates other agents, or a delegated child
// that builds and tests, can honestly take longer than any prompt
// would, but it never goes quiet for long: every tool call and every
// chunk of text is a sign of life. What the timeout catches is an
// agent that hung, not one that is busy.
package idle

import (
	"context"
	"sync"
	"time"
)

// idleContext is the context; its error on expiry is
// context.DeadlineExceeded, like a deadline's.
type idleContext struct {
	context.Context
	done      chan struct{}
	mu        sync.Mutex
	err       error
	timer     *time.Timer
	d         time.Duration
	remaining time.Duration
	due       time.Time
	paused    bool
	epoch     uint64
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
	c := &idleContext{Context: parent, done: make(chan struct{}), d: d}
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
		if c.paused {
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
		if c.err == nil && !c.paused && c.epoch == epoch {
			c.finishLocked(context.DeadlineExceeded)
		}
	})
}

func (c *idleContext) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.paused {
		return
	}
	c.remaining = max(0, time.Until(c.due))
	c.paused = true
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
	c.arm(c.remaining)
}

func (c *idleContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(err)
}

func (c *idleContext) finishLocked(err error) {
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	close(c.done)
}

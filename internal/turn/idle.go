package turn

import (
	"context"
	"sync"
	"time"
)

// idleContext ends after a stretch of silence rather than after a fixed
// total. A turn that coordinates other agents — delegating, awaiting,
// merging — can honestly take longer than any prompt would, but it never
// goes quiet for long: every tool call and every chunk of text is a
// sign of life. What the timeout catches is an agent that hung, not one
// that is busy. Its error is context.DeadlineExceeded, as before.
type idleContext struct {
	context.Context
	done  chan struct{}
	mu    sync.Mutex
	err   error
	timer *time.Timer
	d     time.Duration
}

// withIdleTimeout wraps parent; touch resets the clock, stop ends it.
func withIdleTimeout(parent context.Context, d time.Duration) (ctx context.Context, stop func(), touch func()) {
	c := &idleContext{Context: parent, done: make(chan struct{}), d: d}
	c.timer = time.AfterFunc(d, func() { c.finish(context.DeadlineExceeded) })
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
		c.timer.Reset(c.d)
	}
}

func (c *idleContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	close(c.done)
}

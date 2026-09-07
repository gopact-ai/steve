package turn

import (
	"errors"
	"sync"
)

// SealIdle fences every Handle entry, including commands that never enter the
// execution registry. It never waits behind a live request or holds a writer
// lock across maintenance: later requests receive an explicit refusal.
func (c *Coordinator) SealIdle() (func(), error) {
	if !c.requestMu.TryLock() {
		return nil, errors.New("coordinator requests are still in flight")
	}
	if c.maintaining {
		c.requestMu.Unlock()
		return nil, errors.New("coordinator is already under maintenance")
	}
	c.maintaining = true
	c.requestMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { c.requestMu.Lock(); c.maintaining = false; c.requestMu.Unlock() }) }, nil
}

package turn

import (
	"errors"
	"sort"
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

// InFlight names the conversations whose turns have begun and not ended.
// A restart that is waiting reads it to say whose work it is waiting for
// instead of reporting that something, somewhere, is busy.
func (c *Coordinator) InFlight() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for key := range c.cancels {
		id := conversationOfKey(key)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

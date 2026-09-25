package node

import (
	"log/slog"
	"sync"
)

// notices hands connectivity changes and manifest drift to their observers
// on a goroutine of their own, in the order they were posted. Observers
// write history, and a history write can wait on replication for as long
// as the ledger takes: connecting or losing any machine must not wait with
// it. A change is posted where the registry decides it, under its lock, so
// the order observers hear is the order the changes happened in; a
// machine's loss is never heard before its connection.
type notices struct {
	mu      sync.Mutex
	pending []func()
	running bool
	closed  bool
}

// post queues one delivery and starts a drain unless one is running.
func (n *notices) post(deliver func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.pending = append(n.pending, deliver)
	if !n.running {
		n.running = true
		go n.drain()
	}
}

// drain delivers until nothing is queued, one delivery at a time.
func (n *notices) drain() {
	for {
		n.mu.Lock()
		if len(n.pending) == 0 {
			n.running = false
			n.mu.Unlock()
			return
		}
		deliver := n.pending[0]
		n.pending[0] = nil
		n.pending = n.pending[1:]
		n.mu.Unlock()
		deliver()
	}
}

// close drops what is still queued: a closed registry reports nothing more,
// as it reports no loss for the connections Close releases. A delivery
// already under way finishes on its own.
func (n *notices) close() {
	n.mu.Lock()
	dropped := len(n.pending)
	n.pending, n.closed = nil, true
	n.mu.Unlock()
	if dropped > 0 {
		slog.Warn("node: registry closed with connectivity notices not yet delivered", "dropped", dropped)
	}
}

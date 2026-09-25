package node

import (
	"log/slog"
	"sync"
)

// notices hands connectivity changes and manifest drift to their observers
// on a goroutine of their own, in the order they were posted. Observers
// write history, and a history write can wait on replication for as long
// as the ledger takes: connecting or losing any machine must not wait with
// it. A change of a machine's standing, its connection or its loss, is
// posted where the registry decides it, under its lock, so observers hear
// those in the order they happened: a loss is never heard before its
// connection. Manifest drift is posted after the lock is let go. It is
// heard before the connection whose advert showed it, but drift found by
// a refresh has no set order against a loss or reconnection happening at
// the same time.
//
// A history write that does not end leaves observers behind while machines
// keep connecting and dropping. At most noticesHeld wait; past that the
// oldest is dropped, so history loses its oldest stretch rather than what
// the machines are now. Every kind of notice only records history, except
// a connection's arrival, which also sets the machine up (see the app's
// node observer); a dropped arrival leaves that to the machine's next
// connection, or to the next change of skills for shipping them.
type notices struct {
	mu      sync.Mutex
	pending []func()
	running bool
	closed  bool
	dropped int
}

// noticesHeld is how many notices wait for observers at most. History keeps
// its last thousand observations; a longer queue would only write what
// history then forgets.
const noticesHeld = 1024

// post queues one delivery and starts a drain unless one is running.
func (n *notices) post(deliver func()) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	dropped := 0
	if len(n.pending) == noticesHeld {
		n.pending[0] = nil
		n.pending = n.pending[1:]
		n.dropped++
		dropped = n.dropped
	}
	n.pending = append(n.pending, deliver)
	if !n.running {
		n.running = true
		go n.drain()
	}
	n.mu.Unlock()
	if dropped > 0 {
		slog.Warn("node: observers are behind; dropped the oldest connectivity notice", "dropped", dropped, "held", noticesHeld)
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

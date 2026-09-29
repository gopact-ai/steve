package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// replicaConfirmation is what a quorum has confirmed of the local replica,
// beyond what the replica itself shows.
//
// A replica whose log entries stop arriving while heartbeats still do
// shows nothing amiss: it keeps naming the coordinator, writer generation
// and members it last applied, and it hears the consensus leader. So while
// a node that does not lead consensus runs a business generation or keeps
// a worker tunnel open, it confirms its replica with a quorum every
// ApplyTimeout: it asks the leader for a read index and waits, at most
// ApplyTimeout, for the state machine of its replica to have applied its
// log up to it. The runtime loop and the worker tunnels share these
// confirmations. The generation is given up, and the tunnels close, when a
// confirmation fails, or when none has succeeded for three ApplyTimeouts.
// A confirmation appends nothing to the consensus log but the barrier a
// leader completes once in each term, which the runtime's own quorum reads
// have usually completed already. The leader needs none: its replica holds
// every committed entry, and it gives its generation and tunnels up as
// soon as it stops leading without knowing another leader.
type replicaConfirmation struct {
	mu sync.Mutex
	// confirmed is when the latest confirmation that succeeded started:
	// a quorum read that started the business generation or opened a
	// worker tunnel, a read index this replica then caught up with, or an
	// observation of this node leading consensus.
	confirmed time.Time
	// failure is why the latest confirmation failed, if it started after
	// confirmed, at failedFrom; nil otherwise.
	failure    error
	failedFrom time.Time
	// confirming is set while a confirmation runs, and confirmations
	// counts it until it returns. Once stopped is set none starts.
	confirming    bool
	confirmations sync.WaitGroup
	stopped       bool
}

// replicaConfirmed records seen, an observation of the local replica, and
// reports why a quorum no longer confirms the replica, or nil while one
// does. It also reports whether the caller is to start the confirmation
// the replica is due for, with startConfirmation.
func (r *Runtime) replicaConfirmed(seen observation) (confirm bool, err error) {
	timeout := r.config.Coordination.ApplyTimeout
	c := &r.confirmation
	c.mu.Lock()
	defer c.mu.Unlock()
	if seen.LeaderID == r.config.Coordination.NodeID {
		if seen.at.After(c.confirmed) {
			c.confirmed = seen.at
		}
		c.failure = nil
		return false, nil
	}
	if c.failure != nil {
		return false, c.failure
	}
	unconfirmed := seen.at.Sub(c.confirmed)
	if unconfirmed > 3*timeout {
		return false, fmt.Errorf("%w: no quorum has confirmed this replica for %s", coordination.ErrUnavailable, unconfirmed.Round(time.Millisecond))
	}
	if unconfirmed >= timeout && !c.confirming && !c.stopped {
		c.confirming = true
		c.confirmations.Add(1)
		return true, nil
	}
	return false, nil
}

// startConfirmation runs, in the background, the confirmation that
// replicaConfirmed reported due.
func (r *Runtime) startConfirmation() {
	go func() {
		defer r.confirmation.confirmations.Done()
		r.confirmReplica()
	}()
}

// confirmReplica confirms the local replica with a quorum: it asks the
// consensus leader for a read index and waits for the replica to apply its
// log up to it, within ApplyTimeout in all.
func (r *Runtime) confirmReplica() {
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.ctx, r.config.Coordination.ApplyTimeout)
	defer cancel()
	index, err := r.readIndex(ctx)
	if err == nil {
		err = r.awaitLog(ctx, index)
	}
	if err != nil {
		err = fmt.Errorf("%w: the replica could not be confirmed with a quorum: %w", coordination.ErrUnavailable, err)
	}
	r.confirmation.mu.Lock()
	defer r.confirmation.mu.Unlock()
	r.confirmation.confirming = false
	r.confirmation.settleLocked(started, err)
}

// stop waits for the confirmation running, if any, and starts no other.
func (c *replicaConfirmation) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.confirmations.Wait()
}

// settle records the result of a confirmation that started at started.
func (c *replicaConfirmation) settle(started time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settleLocked(started, err)
}

func (c *replicaConfirmation) settleLocked(started time.Time, err error) {
	if err != nil {
		if started.After(c.confirmed) {
			c.failure, c.failedFrom = err, started
		}
		return
	}
	if started.After(c.confirmed) {
		c.confirmed = started
	}
	if c.failure != nil && !c.failedFrom.After(started) {
		c.failure = nil
	}
}

// awaitLog waits for the state machine of the local replica to have
// applied its log up to index, so an observation of the replica once
// awaitLog returns sees every entry committed by then. Raft hands entries
// to the state machine before it applies them; awaitLog waits for the
// state machine, however long it takes to apply them.
func (r *Runtime) awaitLog(ctx context.Context, index uint64) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if r.service.StateHolds(index) {
			return nil
		}
		select {
		case <-ctx.Done():
			if r.ctx.Err() != nil {
				return ErrInactive
			}
			return fmt.Errorf("%w: this replica did not apply its log up to read index %d within %s; it has applied %d", coordination.ErrUnavailable, index, r.config.Coordination.ApplyTimeout, r.service.Status().AppliedIndex)
		case <-ticker.C:
		}
	}
}

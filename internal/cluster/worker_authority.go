package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// workerAuthorityInterval is how often a worker tunnel compares the
// authority it was opened under with the local replica.
const workerAuthorityInterval = 100 * time.Millisecond

// workerAuthority is what keeps the worker tunnels of this node open, on
// either side of them, beyond the quorum read each was opened on.
//
// The local replica shows when the coordinator, its writer generation or
// the members change, and whether it still hears a consensus leader; the
// tunnels judge it as the runtime loop judges the business generation.
// They share one judgment, which also admits new tunnels, so a tunnel is
// never admitted on a view that has just closed another.
//
// A replica whose log entries stop arriving while heartbeats still do
// shows none of this: it keeps naming the coordinator it last applied.
// Its commands, file writes and agents need no check of their own on the
// worker, so while any tunnel is open a node that does not lead consensus
// confirms its replica with a quorum every ApplyTimeout: it asks the
// leader for a read index and waits, at most ApplyTimeout, for its replica
// to apply its log up to it. The tunnels close when a confirmation fails,
// or when none has succeeded for three ApplyTimeouts. A confirmation
// appends nothing to the consensus log. The leader needs none: its replica
// holds every committed entry, and it gives the tunnels up as soon as it
// stops leading without knowing another leader.
type workerAuthority struct {
	mu   sync.Mutex
	live liveness
	// confirmed is when the latest confirmation that succeeded started:
	// a quorum read that opened a tunnel, a read index this replica then
	// caught up with, or an observation of this node leading consensus.
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

// workerGrant is the authority a worker tunnel was opened under: the
// coordinator assignment and writer generation a quorum read confirmed,
// for the worker on one machine.
type workerGrant struct {
	runtime    *Runtime
	worker     string
	assignment coordination.Assignment
	writer     uint64
}

// grantWorker admits a worker tunnel under the assignment and writer
// generation that a quorum read, asked at asked, found in state. The local
// replica first catches up with that read, since it keeps the tunnel from
// then on, and must be recent enough to keep it by: a replica that stopped
// hearing the consensus leader admits no tunnel until it hears one again.
func (r *Runtime) grantWorker(ctx context.Context, asked time.Time, state coordination.State, worker string) (workerGrant, error) {
	if _, err := r.awaitApplied(ctx, state.AppliedIndex, 0); err != nil {
		return workerGrant{}, err
	}
	r.workers.settle(asked, nil)
	grant := workerGrant{runtime: r, worker: worker, assignment: state.Coordinator, writer: state.WriterGeneration}
	if err := grant.check(); err != nil {
		return workerGrant{}, err
	}
	return grant, nil
}

// check reports why the local replica no longer keeps the grant, or nil
// while it does. It starts the confirmation the replica is due for.
func (g *workerGrant) check() error {
	seen := observation{Status: g.runtime.service.Status(), log: g.runtime.service.LogProgress(), at: time.Now()}
	confirm, err := g.runtime.authorizesWorker(seen, g.worker, g.assignment, g.writer)
	if confirm {
		go func() {
			defer g.runtime.workers.confirmations.Done()
			g.runtime.confirmWorkers()
		}()
	}
	return err
}

// authorizesWorker reports why seen no longer lets the coordinator named
// by assignment, at writer generation writer, drive the worker on machine
// worker, or nil while it still does. It records seen in the judgment the
// node's tunnels share, and reports whether the caller is to start a
// confirmation of the replica.
func (r *Runtime) authorizesWorker(seen observation, worker string, assignment coordination.Assignment, writer uint64) (confirm bool, err error) {
	if !seen.Healthy {
		return false, fmt.Errorf("%w: the consensus replica is no longer healthy", coordination.ErrUnavailable)
	}
	if seen.Coordinator != assignment {
		return false, fmt.Errorf("%w: the coordinator is now %s at epoch %d", coordination.ErrStaleEpoch, seen.Coordinator.NodeID, seen.Coordinator.Epoch)
	}
	if seen.WriterGeneration != writer {
		return false, fmt.Errorf("%w: the writer generation is now %d", coordination.ErrStaleWriter, seen.WriterGeneration)
	}
	if _, ok := seen.Members[worker]; !ok {
		return false, fmt.Errorf("%w: %s is no longer a member", coordination.ErrInvalid, worker)
	}
	return r.keepsWorkers(seen)
}

// keepsWorkers records seen in the judgment the node's tunnels share and
// reports why it no longer keeps them, or whether a confirmation is due.
func (r *Runtime) keepsWorkers(seen observation) (confirm bool, err error) {
	timeout := r.config.Coordination.ApplyTimeout
	w := &r.workers
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := r.judge(&w.live, seen); err != nil {
		return false, err
	}
	if seen.LeaderID == r.config.Coordination.NodeID {
		if seen.at.After(w.confirmed) {
			w.confirmed = seen.at
		}
		w.failure = nil
		return false, nil
	}
	if w.failure != nil {
		return false, w.failure
	}
	unconfirmed := seen.at.Sub(w.confirmed)
	if unconfirmed > 3*timeout {
		return false, fmt.Errorf("%w: no quorum has confirmed this replica for %s", coordination.ErrUnavailable, unconfirmed.Round(time.Millisecond))
	}
	if unconfirmed >= timeout && !w.confirming && !w.stopped {
		w.confirming = true
		w.confirmations.Add(1)
		return true, nil
	}
	return false, nil
}

// confirmWorkers confirms the local replica with a quorum for the node's
// worker tunnels: it asks the consensus leader for a read index and waits
// for the replica to apply its log up to it, within ApplyTimeout in all.
func (r *Runtime) confirmWorkers() {
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
	r.workers.mu.Lock()
	defer r.workers.mu.Unlock()
	r.workers.confirming = false
	r.workers.settleLocked(started, err)
}

// stop waits for the confirmation running, if any, and starts no other.
func (w *workerAuthority) stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.confirmations.Wait()
}

// settle records the result of a confirmation that started at started.
func (w *workerAuthority) settle(started time.Time, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settleLocked(started, err)
}

func (w *workerAuthority) settleLocked(started time.Time, err error) {
	if err != nil {
		if started.After(w.confirmed) {
			w.failure, w.failedFrom = err, started
		}
		return
	}
	if started.After(w.confirmed) {
		w.confirmed = started
	}
	if w.failure != nil && !w.failedFrom.After(started) {
		w.failure = nil
	}
}

// awaitLog waits for the local replica to have applied its log up to
// index. Raft hands entries to the state machine in order, so the next
// observation of the replica includes them.
func (r *Runtime) awaitLog(ctx context.Context, index uint64) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		applied := r.service.LogProgress().Applied
		if applied >= index {
			return nil
		}
		select {
		case <-ctx.Done():
			if r.ctx.Err() != nil {
				return ErrInactive
			}
			return fmt.Errorf("%w: this replica did not apply its log up to read index %d within %s; it has applied %d", coordination.ErrUnavailable, index, r.config.Coordination.ApplyTimeout, applied)
		case <-ticker.C:
		}
	}
}

// running is the business generation running here: its assignment and
// writer generation. It is an error for none to be running, or for the
// replica to be restoring a snapshot, which ends it.
func (r *Runtime) running() (coordination.Assignment, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current
	if r.closed || r.restoring || current == nil || current.restore != r.restores || current.Context.Err() != nil || current.WriterGeneration == 0 {
		return coordination.Assignment{}, 0, ErrInactive
	}
	return current.Assignment, current.WriterGeneration, nil
}

// generationDone is closed when the business generation running for
// assignment and writer generation writer ends. It is an error for no
// such generation to be running here, as it is for running.
func (r *Runtime) generationDone(assignment coordination.Assignment, writer uint64) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current
	if r.closed || r.restoring || current == nil || current.restore != r.restores || current.Context.Err() != nil || current.Assignment != assignment || current.WriterGeneration != writer {
		return nil, ErrInactive
	}
	return current.Context.Done(), nil
}

// watchWorkerAuthority closes a worker tunnel once its grant lapses: when
// the local replica names another coordinator or writer generation, no
// longer lists the worker's machine, stops being recent enough to tell, or
// cannot be confirmed with a quorum (see workerAuthority), and, on the
// coordinator's side, as soon as the business generation that opened it
// ends. It returns without closing when ctx ends or done is closed.
func (p *Peer) watchWorkerAuthority(ctx context.Context, grant workerGrant, done, ended <-chan struct{}, closeConnection func()) {
	ticker := time.NewTicker(workerAuthorityInterval)
	defer ticker.Stop()
	for {
		var lapsed error
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ended:
			lapsed = fmt.Errorf("%w: the business generation that opened it ended", ErrInactive)
		case <-ticker.C:
			if p.Runtime.Load() != grant.runtime {
				lapsed = fmt.Errorf("%w: the consensus runtime was replaced", coordination.ErrUnavailable)
			} else {
				lapsed = grant.check()
			}
		}
		if lapsed != nil {
			slog.Info("cluster: worker tunnel closed", "node", p.Config.NodeID, "worker", grant.worker, "coordinator", grant.assignment.NodeID, "epoch", grant.assignment.Epoch, "writer_generation", grant.writer, "cause", lapsed.Error())
			closeConnection()
			return
		}
	}
}

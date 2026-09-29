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
// never admitted on a view that has just closed another. A node that does
// not lead consensus keeps them only while a quorum confirms its replica,
// as it keeps its business generation; see replicaConfirmation.
type workerAuthority struct {
	mu   sync.Mutex
	live liveness
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
	r.confirmation.settle(asked, nil)
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
		g.runtime.startConfirmation()
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
// reports why it no longer keeps them, or whether a confirmation of the
// replica is due.
func (r *Runtime) keepsWorkers(seen observation) (confirm bool, err error) {
	r.workers.mu.Lock()
	_, err = r.judge(&r.workers.live, seen)
	r.workers.mu.Unlock()
	if err != nil {
		return false, err
	}
	return r.replicaConfirmed(seen)
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
// cannot be confirmed with a quorum (see replicaConfirmation), and, on the
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

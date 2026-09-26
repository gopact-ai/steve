package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// workerAuthorityInterval is how often a worker tunnel compares the
// authority it was opened under with the local replica.
const workerAuthorityInterval = 100 * time.Millisecond

// workerGrant is the authority a worker tunnel was opened under: the
// coordinator assignment and writer generation a quorum read confirmed,
// for the worker on one machine. It is kept by the local replica alone,
// judged as the runtime judges its business generation.
type workerGrant struct {
	runtime    *Runtime
	worker     string
	assignment coordination.Assignment
	writer     uint64
	live       liveness
}

// grantWorker admits a worker tunnel under the assignment and writer
// generation that a quorum read found in state. The local replica first
// catches up with that read, since the tunnel is kept by it from then on,
// and must be recent enough to keep it by: a replica that stopped hearing
// the consensus leader admits no tunnel until it hears one again.
func (r *Runtime) grantWorker(ctx context.Context, state coordination.State, worker string) (workerGrant, error) {
	if _, err := r.awaitApplied(ctx, state.AppliedIndex, 0); err != nil {
		return workerGrant{}, err
	}
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()
	grant := workerGrant{runtime: r, worker: worker, assignment: state.Coordinator, writer: state.WriterGeneration, live: live}
	if err := grant.check(); err != nil {
		return workerGrant{}, err
	}
	return grant, nil
}

// check reports why the local replica no longer keeps the grant, or nil
// while it does.
func (g *workerGrant) check() error {
	seen := observation{Status: g.runtime.service.Status(), log: g.runtime.service.LogProgress(), at: time.Now()}
	return g.runtime.authorizesWorker(&g.live, seen, g.worker, g.assignment, g.writer)
}

// authorizesWorker reports why seen no longer lets the coordinator named
// by assignment, at writer generation writer, drive the worker on machine
// worker, or nil while it still does. It records seen in l.
func (r *Runtime) authorizesWorker(l *liveness, seen observation, worker string, assignment coordination.Assignment, writer uint64) error {
	if !seen.Healthy {
		return fmt.Errorf("%w: the consensus replica is no longer healthy", coordination.ErrUnavailable)
	}
	if _, err := r.judge(l, seen); err != nil {
		return err
	}
	if seen.Coordinator != assignment {
		return fmt.Errorf("%w: the coordinator is now %s at epoch %d", coordination.ErrStaleEpoch, seen.Coordinator.NodeID, seen.Coordinator.Epoch)
	}
	if seen.WriterGeneration != writer {
		return fmt.Errorf("%w: the writer generation is now %d", coordination.ErrStaleWriter, seen.WriterGeneration)
	}
	if _, ok := seen.Members[worker]; !ok {
		return fmt.Errorf("%w: %s is no longer a member", coordination.ErrInvalid, worker)
	}
	return nil
}

// generationDone is closed when the business generation running for
// assignment and writer generation writer ends. It is an error for no
// such generation to be running here.
func (r *Runtime) generationDone(assignment coordination.Assignment, writer uint64) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current
	if current == nil || current.Context.Err() != nil || current.Assignment != assignment || current.WriterGeneration != writer {
		return nil, ErrInactive
	}
	return current.Context.Done(), nil
}

// watchWorkerAuthority closes a worker tunnel once its grant lapses: when
// the local replica names another coordinator or writer generation, no
// longer lists the worker's machine, or stops being recent enough to tell,
// and, on the coordinator's side, as soon as the business generation that
// opened it ends. It reads nothing from other nodes and appends nothing to
// the consensus log. It returns without closing when ctx ends or done is
// closed.
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

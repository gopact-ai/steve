package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
)

type replicaOnly struct{}

func (replicaOnly) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	return ledger.ReplicaPosition{}, ErrInactive
}
func (replicaOnly) Propose(context.Context, ledger.ReplicatedWrite) ([]byte, error) {
	return nil, ErrInactive
}

type application struct{ runtime *Runtime }

func (a application) Apply(command coordination.AppliedCommand) ([]byte, error) {
	result, err := a.runtime.book.ApplyReplicated(command.ID, command.Version, command.Payload)
	if err != nil {
		a.runtime.invalidate(err, false)
	}
	return result, err
}
func (a application) Snapshot() ([]byte, error) { return a.runtime.book.SnapshotReplica() }
func (a application) SnapshotCheckpoint(floor uint64) (coordination.Checkpoint, error) {
	checkpoint, err := a.runtime.book.SnapshotReplicaCheckpoint(floor)
	if err != nil {
		return nil, err
	}
	return checkpoint, nil
}
func (a application) Restore(data []byte) error {
	// Do not call Service here: Raft holds its FSM mutex during Restore.
	a.runtime.mu.Lock()
	a.runtime.restoring = true
	a.runtime.mu.Unlock()
	a.runtime.invalidate(ErrInactive, true)
	err := a.runtime.book.RestoreReplica(data)
	a.runtime.mu.Lock()
	a.runtime.restoring = false
	if err != nil {
		a.runtime.lastError = err
	}
	a.runtime.notifyLocked()
	a.runtime.mu.Unlock()
	return err
}

type replicator struct {
	runtime    *Runtime
	generation *generation
	book       *ledger.Ledger
}

func (b *replicator) boundContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(b.generation.Context, cancel)
	return ctx, func() { stop(); cancel() }
}

func (b *replicator) Prepare(parent context.Context) (ledger.ReplicaPosition, error) {
	if !b.runtime.valid(b.generation) {
		return ledger.ReplicaPosition{}, ErrInactive
	}
	ctx, cancel := b.boundContext(parent)
	defer cancel()
	state, err := b.runtime.ReadState(ctx)
	if err != nil {
		return ledger.ReplicaPosition{}, err
	}
	if state.Coordinator != b.generation.Assignment {
		err := errors.Join(ErrInactive, coordination.ErrStaleEpoch)
		b.runtime.revoke(b.generation, err)
		return ledger.ReplicaPosition{}, err
	}
	if state.Coordinator.NodeID != b.runtime.config.Coordination.NodeID {
		return ledger.ReplicaPosition{}, coordination.ErrNotCoordinator
	}
	if state.WriterGeneration == 0 || state.WriterGeneration != b.generation.WriterGeneration {
		err := errors.Join(ErrInactive, coordination.ErrStaleWriter)
		b.runtime.revoke(b.generation, err)
		return ledger.ReplicaPosition{}, err
	}
	return b.caughtUp(ctx, state)
}

// caughtUp is the rest of Prepare once state, a read of the committed state
// through the leader, has shown this generation writing: it waits for this
// replica to apply what that read saw. ctx is bound to the generation.
func (b *replicator) caughtUp(ctx context.Context, state coordination.State) (ledger.ReplicaPosition, error) {
	// Ledger and task store writers hold their locks through Prepare, and a
	// caller may have no deadline. Catching up is bounded by ApplyTimeout so
	// a replica that stays behind fails this write as unavailable instead of
	// stalling every writer queued behind it. Nothing has been proposed, so
	// this path does not revoke the generation and the write can be retried.
	if _, err := b.runtime.awaitApplied(ctx, state.AppliedIndex, state.AppVersion); err != nil {
		return ledger.ReplicaPosition{}, err
	}
	if !b.runtime.valid(b.generation) {
		return ledger.ReplicaPosition{}, ErrInactive
	}
	version, err := b.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: state.Coordinator.Epoch}, err
}

func (b *replicator) Propose(parent context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	if !b.runtime.valid(b.generation) || write.CoordinatorEpoch != b.generation.Assignment.Epoch {
		return nil, ErrInactive
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	// Submission and the wait for local apply do not observe the caller's
	// cancellation. Submission on this node is bounded by coordination's
	// barrier and apply timeouts; submission routed to the leader by the
	// client's retry window plus one attempt's timeout, since an attempt
	// started before the window ends runs to its own timeout (with the
	// defaults, about ten seconds). The wait for local apply gives up after
	// ApplyTimeout. The generation's context bounds both. Any failure after
	// submission revokes the generation.
	ctx, cancel := b.boundContext(context.WithoutCancel(parent))
	defer cancel()
	command := coordination.AppCommand{ID: write.ID, CallerNodeID: b.generation.NodeID, CoordinatorEpoch: write.CoordinatorEpoch, ExpectedVersion: write.ExpectedVersion, WriterGeneration: b.generation.WriterGeneration, Payload: write.Payload}
	result, err := b.runtime.propose(ctx, command)
	if err != nil {
		// A timeout can hide a committed document replacement. Discard this
		// generation's caches before any later mutation can overwrite it.
		b.runtime.revoke(b.generation, err)
		return nil, err
	}
	if _, err := b.runtime.awaitApplied(ctx, result.Index, result.AppVersion); err != nil {
		// The write is committed; this generation's caches show it only
		// once this replica applies it. Callers take an unavailable error
		// for a write that did not happen, not for an unknown outcome, and
		// may record that verdict with a later write. The revoke below,
		// made before this returns, fails every later write of this
		// generation with ErrInactive, so no such verdict is recorded; it
		// must stay ahead of the return.
		if errors.Is(err, coordination.ErrUnavailable) {
			err = fmt.Errorf("%w; write %s was committed", err, write.ID)
		}
		b.runtime.revoke(b.generation, err)
		return nil, err
	}
	return result.Data, nil
}

// awaitApplied is waitApplied bounded by ApplyTimeout. Giving up is reported
// as coordination.ErrUnavailable; see applyGaveUp. The caller's own
// cancellation or deadline is returned unchanged.
func (r *Runtime) awaitApplied(ctx context.Context, index, version uint64) (uint64, error) {
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, r.config.Coordination.ApplyTimeout)
	defer cancel()
	local, err := r.waitApplied(bounded, index, version)
	return local, r.applyGaveUp(ctx, err, started, index, version)
}

// applyGaveUp is err, which ended a wait since started for this replica to
// apply index and version, unless a limit this runtime set within parent
// ended the wait: then it is coordination.ErrUnavailable naming what was
// waited for, how far this replica got and how long it waited. Once parent
// has ended, its cancellation or deadline is returned unchanged.
func (r *Runtime) applyGaveUp(parent context.Context, err error, started time.Time, index, version uint64) error {
	if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
		return err
	}
	progress := r.service.Status()
	reached := fmt.Sprintf("applied index %d, application version %d", progress.AppliedIndex, progress.AppVersion)
	if book, readErr := r.book.ReplicaVersion(); readErr == nil {
		reached += fmt.Sprintf(", ledger version %d", book)
	}
	return fmt.Errorf("%w: local replica did not reach applied index %d, application version %d within %s; it has %s", coordination.ErrUnavailable, index, version, time.Since(started).Round(time.Millisecond), reached)
}

// readGaveUp is err, which ended a quorum read, unless the limit of
// ApplyTimeout that readAssignment sets, within parent, on a read it asks
// the consensus leader for ended it: then it is coordination.ErrUnavailable
// saying so. Once parent has ended, its cancellation or deadline is
// returned unchanged. A read on this node that gives up reports itself
// unavailable and says why on its own.
func (r *Runtime) readGaveUp(parent context.Context, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, coordination.ErrUnavailable) || parent.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: the quorum read of the coordinator assignment did not finish within %s", coordination.ErrUnavailable, r.config.Coordination.ApplyTimeout)
}

func (r *Runtime) waitApplied(ctx context.Context, index, version uint64) (uint64, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		state := r.service.Status()
		if !state.Healthy {
			return 0, coordination.ErrApplication
		}
		local, err := r.book.ReplicaVersion()
		if err != nil {
			return 0, fmt.Errorf("read local replica progress: %w", err)
		}
		if state.AppliedIndex >= index && state.AppVersion >= version && local >= version {
			return local, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-r.ctx.Done():
			return 0, ErrInactive
		case <-ticker.C:
		}
	}
}

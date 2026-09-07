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
	if _, err := b.runtime.waitApplied(ctx, state.AppliedIndex, state.AppVersion); err != nil {
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
	ctx, cancel := b.boundContext(parent)
	defer cancel()
	command := coordination.AppCommand{ID: write.ID, CallerNodeID: b.generation.NodeID, CoordinatorEpoch: write.CoordinatorEpoch, ExpectedVersion: write.ExpectedVersion, WriterGeneration: b.generation.WriterGeneration, Payload: write.Payload}
	result, err := b.runtime.propose(ctx, command)
	if err != nil {
		// A timeout can hide a committed document replacement. Discard this
		// generation's caches before any later mutation can overwrite it.
		b.runtime.revoke(b.generation, err)
		return nil, err
	}
	if _, err := b.runtime.waitApplied(ctx, result.Index, result.AppVersion); err != nil {
		b.runtime.revoke(b.generation, err)
		return nil, err
	}
	return result.Data, nil
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

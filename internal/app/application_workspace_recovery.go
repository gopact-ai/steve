package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
)

type recoverySessionCloser interface {
	CloseRecoverySession(context.Context, harness.Placement, string, string) (nodewire.SessionState, error)
}

type applicationWorkspaceRecovery struct {
	mu        sync.Mutex
	attempts  *attempt.Service
	artifacts *artifact.Store
	state     *state.Store
	sessions  recoverySessionCloser
	control   *turn.WorkspaceRecoveryControl
}

// Recovery work has its own bounded reconciler, not the stop batch's deadline.
// Each episode uses the same driver lease as copy preparation.
func (r *applicationWorkspaceRecovery) Reconcile(parent context.Context) error {
	if !r.mu.TryLock() {
		return nil
	}
	defer r.mu.Unlock()
	all, err := r.attempts.WorkspaceRecoveries(parent)
	if err != nil {
		return err
	}
	var failures []error
	for _, episode := range all {
		ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
		err := r.control.Drive(ctx, episode.ID, func(ctx context.Context, driver ledger.Lease) error { return r.drain(ctx, episode.ID, driver) })
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("workspace recovery %s: %w", episode.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (r *applicationWorkspaceRecovery) drain(ctx context.Context, id string, driver ledger.Lease) error {
	episode, err := r.attempts.EnrollRecoveryNatives(ctx, id, false, driver)
	if err != nil {
		return err
	}
	if err := r.retire(ctx, episode, false, driver); err != nil {
		return err
	}
	episode, err = r.attempts.BeginRecoveryDrain(ctx, id, driver)
	if err != nil {
		return err
	}
	episode, err = r.attempts.PublishRecoveryHead(ctx, id, r.artifacts.RecoveryOutputTx)
	if err != nil {
		return err
	}
	if episode.Producer != nil {
		return attempt.ErrWorkspaceRecovery
	}
	episode, err = r.attempts.EnrollRecoveryNatives(ctx, id, true, driver)
	if err != nil {
		return err
	}
	if err := r.retire(ctx, episode, true, driver); err != nil {
		return err
	}
	_, err = r.artifacts.CaptureRecoveryResidual(ctx, id, driver)
	return err
}

func (r *applicationWorkspaceRecovery) retire(ctx context.Context, episode attempt.WorkspaceRecovery, copy bool, driver ledger.Lease) error {
	for _, native := range episode.NativeRetirements {
		if native.Copy != copy {
			continue
		}
		if native.Session == "" {
			return errors.New("recovery native preparation has no confirmed open or process-stop identity")
		}
		current, err := r.attempts.Get(ctx, native.Binding.AttemptID)
		if err != nil {
			return err
		}
		if !current.State.Terminal() || current.Unsettled || current.SessionSettled == nil || !*current.SessionSettled {
			return attempt.ErrStopConfirmationRequired
		}
		if native.RetiredAt.IsZero() {
			if err := r.state.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return r.attempts.RetireRecoveryNativeTx(tx, episode.ID, native, driver) }); err != nil {
				return err
			}
		}
		if native.Proof != nil {
			continue
		}
		adopted, err := r.attempts.AdoptRecoveryNativeStop(ctx, episode.ID, native, driver)
		if err != nil {
			return err
		}
		if adopted {
			continue
		}
		// Do not interfere with an original abandoned source: its stop owner
		// still owes the formal stop receipt, projection and delivery.
		if current.Abandoned != nil {
			return attempt.ErrStopConfirmationRequired
		}
		workdir := episode.Target.Path
		if copy {
			workdir = episode.Workspace.Path
		}
		closeCtx := execution.WithProbeKey(ctx, execution.Key{TaskID: current.TaskID, InstanceID: current.TurnID, AttemptID: current.ID})
		proof, err := r.sessions.CloseRecoverySession(closeCtx, harness.Placement{Node: native.Binding.NodeID, Harness: native.Harness}, native.Session, workdir)
		if err != nil {
			return err
		}
		if err := r.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: proof}); err != nil {
			return err
		}
	}
	return nil
}

package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func checkRecoveryDriverTx(tx *ledger.Tx, id string, driver ledger.Lease) (WorkspaceRecovery, error) {
	if driver.Key != "workspace-recovery-driver:"+id {
		return WorkspaceRecovery{}, ledger.ErrStale
	}
	if err := tx.CheckLocalLease(driver); err != nil {
		return WorkspaceRecovery{}, err
	}
	r, err := recoveryByIDTx(tx, id)
	if err != nil {
		return r, err
	}
	return r, checkRecoveryDeclarationTx(tx, r)
}

// DriveWorkspaceRecovery serializes draining and preparation on the same
// renewable fencing row. Its context is cancelled on driver lease loss.
type recoveryDriverLifetimeKey struct{}
type recoveryDriverLifetime struct{ context.Context }

// DriveWorkspaceRecovery keeps work cancellation separate from the actual
// service/lease lifetime. Only an admitted WAL consumes the detached lifetime.
func (s *Service) DriveWorkspaceRecovery(parent, lifetime context.Context, id string, run func(context.Context, ledger.Lease) error) error {
	lease, err := s.l.Acquire(parent, "workspace-recovery-driver:"+id, NewID(), s.TTL)
	if err != nil {
		return err
	}
	life, endLife := context.WithCancelCause(lifetime)
	ctx, cancel := context.WithCancelCause(parent)
	stopWork := context.AfterFunc(life, func() { cancel(context.Cause(life)) })
	ctx = context.WithValue(ctx, recoveryDriverLifetimeKey{}, recoveryDriverLifetime{life})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(max(s.TTL/3, time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-tick.C:
				if _, err := s.l.RenewAny(life, lease, s.TTL); err != nil {
					endLife(err)
					return
				}
			}
		}
	}()
	defer func() {
		endLife(context.Canceled)
		<-done
		stopWork()
		cancel(context.Canceled)
		s.release(context.WithoutCancel(parent), lease)
	}()
	return run(ctx, lease)
}

func RecoveryDriverLifetime(ctx context.Context) context.Context {
	if lifetime, ok := ctx.Value(recoveryDriverLifetimeKey{}).(recoveryDriverLifetime); ok {
		return lifetime.Context
	}
	return ctx
}

func originalRecoveryStoppedTx(tx *ledger.Tx, r WorkspaceRecovery) error {
	for _, source := range r.Sources {
		original, err := GetTx(tx, source.Attempt)
		if err != nil {
			return err
		}
		if original.Abandoned == nil || original.Abandoned.ProjectedAt.IsZero() || original.Unsettled || original.StopEvidence != "task-stop/"+original.ID && original.StopEvidence != "process-stop/"+original.ID {
			return ErrStopConfirmationRequired
		}
		tracked, err := nativeTaskTx(tx, original)
		if err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, taskStopReceiptKind, original.ID).Scan(&raw); err != nil {
			return err
		}
		var receipt TaskStopReceipt
		if json.Unmarshal(raw, &receipt) != nil || receipt.AttemptID != original.ID || receipt.Evidence.ObservedAt.IsZero() || !receipt.Evidence.Session.ProcessStopped || !matchesStoppedSession(original, tracked, receipt.Evidence.Session) {
			return ErrStopConfirmationRequired
		}
	}
	return nil
}

func recoveryPhysicalRecordsTx(tx ledger.Reader, r WorkspaceRecovery, copy bool) ([]Record, error) {
	node, directory := r.Target.Node, r.Target.Path
	if copy {
		node, directory = r.Workspace.Node, r.Workspace.Path
	}
	if directory == "" {
		return nil, nil
	}
	var raw string
	if err := tx.QueryRow(`SELECT json_group_array(json_object('id',id,'state',state,'revision',revision,'data',data)) FROM operations WHERE kind=?`, kind).Scan(&raw); err != nil {
		return nil, err
	}
	var envelopes []recoveryEnvelope
	if err := json.Unmarshal([]byte(raw), &envelopes); err != nil {
		return nil, err
	}
	var out []Record
	for _, envelope := range envelopes {
		record, err := decode(ledger.Operation{ID: envelope.ID, Kind: kind, State: envelope.State, Revision: envelope.Revision, Data: json.RawMessage(envelope.Data)})
		if err != nil {
			return nil, err
		}
		if record.Workspace.Node == node && samePhysicalPath(record.Workspace.Path, directory) {
			out = append(out, record)
		}
	}
	return out, nil
}

// EnrollRecoveryNatives recovers exact historical bindings while their tasks
// are still protected by the physical-directory deletion guard. Unknown native
// imports or lost identities are not converted into synthetic close authority.
func (s *Service) EnrollRecoveryNatives(ctx context.Context, id string, copy bool, driver ledger.Lease) (WorkspaceRecovery, error) {
	var result WorkspaceRecovery
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryDriverTx(tx, id, driver)
		if err != nil {
			return err
		}
		records, err := recoveryPhysicalRecordsTx(tx, r, copy)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Session == "" {
				if potentialWriter(record) || record.NativeContext != "" || copy && record.Result != nil {
					return ErrStopConfirmationRequired
				}
				continue
			}
			latest, found, err := LatestForSessionTx(tx, record.Node, record.Harness, record.Session)
			if err != nil {
				return err
			}
			if !found || latest.ID != record.ID {
				continue
			}
			if err := putRecoveryNativeTx(tx, &r, record, copy); err != nil {
				return err
			}
		}
		node, directory := r.Target.Node, r.Target.Path
		if copy {
			node, directory = r.Workspace.Node, r.Workspace.Path
		}
		contexts, err := state.RecoveryContextsTx(tx, node, directory)
		if err != nil {
			return err
		}
		for _, session := range contexts {
			if recoveryNativeIndex(r, session.NodeID, session.HarnessID, session.UpstreamID) < 0 {
				return errors.New("recovery context has no exact native execution identity")
			}
		}
		result = r
		return nil
	})
	return result, err
}

func checkRecoveryNativeConvergenceTx(tx *ledger.Tx, r WorkspaceRecovery, copy bool) error {
	records, err := recoveryPhysicalRecordsTx(tx, r, copy)
	if err != nil {
		return err
	}
	for _, record := range records {
		if potentialWriter(record) || abandonProjectionPending(record) {
			return writerRefusal(record)
		}
		if record.Session == "" {
			if record.NativeContext != "" || copy && record.Result != nil {
				return ErrStopConfirmationRequired
			}
			continue
		}
		index := recoveryNativeIndex(r, record.Node, record.Harness, record.Session)
		if index < 0 {
			return ErrStopConfirmationRequired
		}
		n := r.NativeRetirements[index]
		if n.Proof == nil || n.RetiredAt.IsZero() {
			return ErrStopConfirmationRequired
		}
		if err := recoveryNativeProof(n, *n.Proof); err != nil {
			return err
		}
		if _, err := latestRecoveryNativeTx(tx, n); err != nil {
			return err
		}
	}
	for _, native := range r.NativeRetirements {
		if native.Copy != copy {
			continue
		}
		if native.Proof == nil || native.RetiredAt.IsZero() {
			return ErrStopConfirmationRequired
		}
		if err := recoveryNativeProof(native, *native.Proof); err != nil {
			return err
		}
		if err := state.CheckRecoveryRetiredTx(tx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) BeginRecoveryDrain(ctx context.Context, id string, driver ledger.Lease) (WorkspaceRecovery, error) {
	var result WorkspaceRecovery
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryDriverTx(tx, id, driver)
		if err != nil {
			return err
		}
		if err := originalRecoveryStoppedTx(tx, r); err != nil {
			return err
		}
		if err := checkRecoveryNativeConvergenceTx(tx, r, false); err != nil {
			return err
		}
		if r.Phase == "draining" {
			result = r
			return nil
		}
		if r.Phase != "recorded" && r.Phase != "materializing" && r.Phase != "ready" && r.Phase != "working" {
			return ErrWorkspaceRecovery
		}
		r.Phase = "draining"
		if err := saveWorkspaceRecoveryTx(tx, &r, "recovery-draining"); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

func recoveryNativeTasksTx(tx ledger.Reader, r WorkspaceRecovery, ids []string) error {
	for _, copy := range []bool{false, true} {
		records, err := recoveryPhysicalRecordsTx(tx, r, copy)
		if err != nil {
			return err
		}
		for _, record := range records {
			for _, id := range ids {
				if record.TaskID == id {
					return fmt.Errorf("%w: workspace recovery %s retains native cleanup", task.ErrRetirementPending, r.ID)
				}
			}
		}
	}
	return nil
}

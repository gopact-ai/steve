package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// RecoveryNative retains cleanup authority even for an unchanged copy turn.
// Proof is the authenticated node outcome, not command settlement or an owed
// close disappearing. RetiredAt separately records the context tombstone.
type RecoveryNative struct {
	Copy         bool                    `json:"copy"`
	Session      string                  `json:"session"`
	Context      string                  `json:"context,omitempty"`
	Harness      string                  `json:"harness"`
	Binding      nodewire.SessionBinding `json:"binding"`
	Command      string                  `json:"command"`
	Conversation string                  `json:"conversation"`
	Agent        string                  `json:"agent"`
	Proof        *RetainedEvidence       `json:"proof,omitempty"`
	RetiredAt    time.Time               `json:"retired_at,omitempty"`
}

func recoveryNativeTx(tx ledger.Reader, record Record, copy bool) (RecoveryNative, error) {
	if record.Execution == nil || record.Execution.TaskID != record.TaskID || record.Session == "" {
		return RecoveryNative{}, ErrStopConfirmationRequired
	}
	tracked, found, err := task.GetTx(tx, record.TaskID)
	if err != nil {
		return RecoveryNative{}, err
	}
	if !found {
		return RecoveryNative{}, errors.New("recovery native cleanup requires its original task")
	}
	binding := nodewire.SessionBinding{NativeImportID: record.NativeImportID(), PluginRuntimeID: record.PluginRuntimeID(), ProjectID: record.Project, SessionID: RetainedSessionID(tracked.Channel, record.TaskID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch}
	return RecoveryNative{Copy: copy, Session: record.Session, Context: record.NativeContext, Harness: record.Harness, Binding: binding, Command: InputCommandID(record), Conversation: tracked.Channel, Agent: record.Agent}, nil
}

func recoveryNativeIndex(r WorkspaceRecovery, node, harness, session string) int {
	for i, n := range r.NativeRetirements {
		if n.Binding.NodeID == node && n.Harness == harness && n.Session == session {
			return i
		}
	}
	return -1
}

func putRecoveryNativeTx(tx *ledger.Tx, r *WorkspaceRecovery, record Record, copy bool) error {
	n, err := recoveryNativeTx(tx, record, copy)
	if err != nil {
		return err
	}
	index := recoveryNativeIndex(*r, record.Node, record.Harness, record.Session)
	if index >= 0 {
		previous := r.NativeRetirements[index]
		if previous.Binding == n.Binding && previous.Context == n.Context {
			return nil
		}
		if previous.Proof != nil || !previous.RetiredAt.IsZero() {
			return ErrWorkspaceRecovery
		}
		r.NativeRetirements[index] = n
	} else {
		r.NativeRetirements = append(r.NativeRetirements, n)
	}
	return saveWorkspaceRecoveryTx(tx, r, "recovery-native")
}

// The identity and its deletion guard commit before any native prompt. A later
// no-change result does not erase this independently owned close obligation.
func trackRecoveryNativeTx(tx *ledger.Tx, before, next Record) error {
	if next.WorkspaceRecovery == nil || next.Session == "" || before.Session == next.Session && before.NativeContext == next.NativeContext {
		return nil
	}
	r, err := recoveryByIDTx(tx, next.WorkspaceRecovery.ID)
	if err != nil {
		return err
	}
	if r.Producer == nil || r.Producer.Attempt != next.ID || next.Execution == nil || r.Producer.Execution != *next.Execution || !sameRecoveryWorkspace(r.Workspace, next.Workspace) || r.Phase != "working" && r.Phase != "draining" {
		return ErrWorkspaceRecovery
	}
	return putRecoveryNativeTx(tx, &r, next, true)
}

func latestRecoveryNativeTx(tx ledger.Reader, n RecoveryNative) (Record, error) {
	r, found, err := LatestForSessionTx(tx, n.Binding.NodeID, n.Harness, n.Session)
	if err != nil {
		return Record{}, err
	}
	if !found || r.ID != n.Binding.AttemptID {
		return Record{}, errors.New("recovery native binding was replaced")
	}
	actual, err := recoveryNativeTx(tx, r, n.Copy)
	if err != nil {
		return r, err
	}
	if actual.Binding != n.Binding || actual.Context != n.Context || actual.Command != n.Command || actual.Harness != n.Harness || r.Session != n.Session {
		return r, ErrStopConfirmationRequired
	}
	return r, nil
}

func recoveryNativeProof(n RecoveryNative, proof RetainedEvidence) error {
	st := proof.Session
	if !nodewire.IsManagedSession(n.Session) || proof.ObservedAt.IsZero() || !st.ProcessStopped || st.ID != n.Session || st.Harness != n.Harness || st.ContextID != n.Context || st.Binding != n.Binding {
		return ErrStopConfirmationRequired
	}
	if st.Command != nil && (st.Command.ID != n.Command || st.Command.InputSequence == 0 || st.Command.InputSequence > st.InputAccepted) {
		return ErrStopConfirmationRequired
	}
	return nil
}

// AcceptRecoveryNativeStop consumes only a fresh exact idle-close receipt.
// It does not finish a task or rewrite its native accounting.
func (s *Service) AcceptRecoveryNativeStop(ctx context.Context, expected WorkspaceRecovery, native RecoveryNative, driver ledger.Lease, proof RetainedEvidence) error {
	if proof.ObservedAt.After(s.now().Add(time.Second)) || s.now().Sub(proof.ObservedAt) > time.Minute {
		return ErrStopConfirmationRequired
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryDriverTx(tx, expected.ID, driver)
		if err != nil {
			return err
		}
		index := recoveryNativeIndex(r, native.Binding.NodeID, native.Harness, native.Session)
		if index < 0 || r.NativeRetirements[index].Binding != native.Binding || r.NativeRetirements[index].Context != native.Context {
			return ledger.ErrConflict
		}
		if _, err := latestRecoveryNativeTx(tx, native); err != nil {
			return err
		}
		if err := recoveryNativeProof(native, proof); err != nil {
			return err
		}
		if r.NativeRetirements[index].Proof != nil {
			return nil
		}
		r.NativeRetirements[index].Proof = &proof
		return saveWorkspaceRecoveryTx(tx, &r, "recovery-process-stopped")
	})
}

// RetireRecoveryNativeTx composes the durable context tombstone with its owner
// receipt. Removing a live slot, by itself, never issues a stop proof.
func (s *Service) RetireRecoveryNativeTx(tx *ledger.Tx, id string, native RecoveryNative, driver ledger.Lease) error {
	r, err := checkRecoveryDriverTx(tx, id, driver)
	if err != nil {
		return err
	}
	index := recoveryNativeIndex(r, native.Binding.NodeID, native.Harness, native.Session)
	if index < 0 || r.NativeRetirements[index].Binding != native.Binding {
		return ledger.ErrConflict
	}
	if _, err := latestRecoveryNativeTx(tx, native); err != nil {
		return err
	}
	if !r.NativeRetirements[index].RetiredAt.IsZero() {
		return nil
	}
	r.NativeRetirements[index].RetiredAt = s.now().UTC()
	return saveWorkspaceRecoveryTx(tx, &r, "recovery-context-retired")
}

// AdoptRecoveryNativeStop consumes an already accepted stop-owner receipt.
// Its observation need not be fresh again: the original owner already admitted
// it under the exact identity. Generic close outcomes cannot issue this fact.
func (s *Service) AdoptRecoveryNativeStop(ctx context.Context, id string, native RecoveryNative, driver ledger.Lease) (bool, error) {
	adopted := false
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryDriverTx(tx, id, driver)
		if err != nil {
			return err
		}
		index := recoveryNativeIndex(r, native.Binding.NodeID, native.Harness, native.Session)
		if index < 0 || r.NativeRetirements[index].Binding != native.Binding {
			return ledger.ErrConflict
		}
		if r.NativeRetirements[index].Proof != nil {
			adopted = true
			return nil
		}
		if _, err := latestRecoveryNativeTx(tx, native); err != nil {
			return err
		}
		var raw []byte
		err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, taskStopReceiptKind, native.Binding.AttemptID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var receipt TaskStopReceipt
		if json.Unmarshal(raw, &receipt) != nil || receipt.AttemptID != native.Binding.AttemptID {
			return ErrStopConfirmationRequired
		}
		if err := recoveryNativeProof(native, receipt.Evidence); err != nil {
			return err
		}
		r.NativeRetirements[index].Proof = &receipt.Evidence
		if err := saveWorkspaceRecoveryTx(tx, &r, "recovery-process-stopped"); err != nil {
			return err
		}
		adopted = true
		return nil
	})
	return adopted, err
}

func validateRecoveryNatives(r WorkspaceRecovery) error {
	seen := map[string]bool{}
	for _, n := range r.NativeRetirements {
		key := n.Binding.NodeID + "\x00" + n.Harness + "\x00" + n.Session
		if seen[key] || n.Session == "" || n.Harness == "" || n.Agent == "" || n.Command == "" || n.Binding.ProjectID != r.Project || n.Binding.TaskID == "" || n.Binding.AttemptID == "" || n.Binding.TaskEpoch == 0 || n.Binding.ExecutionEpoch == 0 || n.Binding.SessionID == "" {
			return errors.New("recovery native identity is incomplete or repeated")
		}
		seen[key] = true
		node := r.Target.Node
		if n.Copy {
			node = r.Workspace.Node
			if r.Workspace.Path == "" {
				return ErrWorkspaceRecovery
			}
		}
		if n.Binding.NodeID != node {
			return ErrStopConfirmationRequired
		}
		if n.Proof != nil {
			if err := recoveryNativeProof(n, *n.Proof); err != nil {
				return err
			}
		}
	}
	return nil
}

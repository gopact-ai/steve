package attempt

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// MarkRecoveryWriting runs before preparing any external runtime for a copy.
// Once true, this obligation cannot be inferred away from a failed command.
func (s *Service) MarkRecoveryWriting(ctx context.Context, id string) error {
	r, err := s.Get(ctx, id)
	if err != nil || r.WorkspaceRecovery == nil {
		return err
	}
	for _, lease := range r.Leases {
		if err := s.l.CheckAny(ctx, lease); err != nil {
			return err
		}
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		current, err := GetTx(tx, id)
		if err != nil {
			return err
		}
		if current.State.Terminal() || current.Unsettled {
			return ErrWorkspaceRecovery
		}
		for _, lease := range current.Leases {
			if lease.Region == "" || lease.Region == s.l.Region() {
				if err := tx.CheckLocalLease(lease); err != nil {
					return err
				}
			}
		}
		if err := task.CheckExecutionTx(tx, current.Execution); err != nil {
			return err
		}
		if err := checkAdmissionTx(tx, current.Spec); err != nil {
			return err
		}
		return markRecoveryWritingTx(tx, current)
	})
}

func markRecoveryWritingTx(tx *ledger.Tx, r Record) error {
	if r.WorkspaceRecovery == nil {
		return nil
	}
	episode, err := recoveryByIDTx(tx, r.WorkspaceRecovery.ID)
	if err != nil {
		return err
	}
	if episode.Phase != "working" && episode.Phase != "draining" || !sameRecoveryWorkspace(episode.Workspace, r.Workspace) || episode.Producer == nil || r.Execution == nil || episode.Producer.Attempt != r.ID || episode.Producer.Execution != *r.Execution || episode.Head.Artifact != r.Base || episode.Head.Version != r.WorkspaceRecovery.HeadVersion {
		return ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, episode); err != nil {
		return err
	}
	if episode.Producer.NativeMayWrite == nil {
		return errors.New("recovery preparation has no durable admission decision")
	}
	if *episode.Producer.NativeMayWrite {
		return nil
	}
	copy := *episode.Producer
	mayWrite := true
	copy.NativeMayWrite = &mayWrite
	episode.Producer = &copy
	if err := saveWorkspaceRecoveryTx(tx, &episode, "recovery-writer"); err != nil {
		return err
	}
	return putRecoveryNativeTx(tx, &episode, r, true)
}

func releaseUnpreparedRecoveryTx(tx *ledger.Tx, expected WorkspaceRecovery, record Record) error {
	current, err := recoveryByIDTx(tx, expected.ID)
	if err != nil {
		return err
	}
	p := current.Producer
	if record.WorkspaceRecovery == nil || record.WorkspaceRecovery.ID != current.ID || !sameRecoveryWorkspace(current.Workspace, record.Workspace) || record.Base != current.Head.Artifact || record.WorkspaceRecovery.HeadVersion != current.Head.Version || p == nil || current.Revision != expected.Revision || p.Attempt != record.ID || record.Execution == nil || p.Execution != *record.Execution || current.Head.Artifact != p.Base || current.Head.Version != p.HeadVersion || p.NativeMayWrite == nil || *p.NativeMayWrite {
		return ErrWorkspaceRecovery
	}
	if !record.State.Terminal() || record.SessionSettled == nil || !*record.SessionSettled || record.Unsettled || record.Session != "" || record.NativeContext != "" || record.Result != nil {
		return ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, current); err != nil {
		return err
	}
	current.Producer = nil
	if current.Phase != "draining" {
		current.Phase = "ready"
	}
	return saveWorkspaceRecoveryTx(tx, &current, "recovery-unprepared")
}

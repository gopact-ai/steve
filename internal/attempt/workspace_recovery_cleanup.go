package attempt

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func CheckRecoveryCleanupTx(tx *ledger.Tx, id string, driver ledger.Lease) (WorkspaceRecovery, error) {
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
	if r.Phase != "released" || r.Result == nil || r.Producer != nil {
		return r, ErrWorkspaceRecovery
	}
	if r.Workspace.Path != "" {
		if err := project.CheckRecoveryWorkspaceTx(tx, r.Workspace); err != nil {
			return r, err
		}
	}
	records, err := recoveryPhysicalRecordsTx(tx, r, true)
	if err != nil {
		return r, err
	}
	for _, record := range records {
		if potentialWriter(record) {
			return r, writerRefusal(record)
		}
	}
	for _, native := range r.NativeRetirements {
		if native.Copy && (native.Proof == nil || native.RetiredAt.IsZero()) {
			return r, ErrStopConfirmationRequired
		}
	}
	return r, nil
}

func (s *Service) RecordRecoveryContainer(ctx context.Context, id string, driver ledger.Lease, identity, rootIdentity string, generation int64) error {
	if identity == "" || rootIdentity == "" || len(identity) > 256 || len(rootIdentity) > 256 || generation < 1 {
		return errors.New("recovery container has no bounded directory identity")
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := CheckRecoveryCleanupTx(tx, id, driver)
		if err != nil {
			return err
		}
		if r.CopyIdentity != "" {
			if r.CopyIdentity != identity || r.CopyGeneration != generation || r.CopyRootIdentity != rootIdentity {
				return ledger.ErrConflict
			}
			return nil
		}
		r.CopyIdentity, r.CopyRootIdentity, r.CopyGeneration = identity, rootIdentity, generation
		return saveWorkspaceRecoveryTx(tx, &r, "recovery-container-removal")
	})
}

func (s *Service) ConfirmRecoveryCopyRemoved(ctx context.Context, id string, driver ledger.Lease) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := CheckRecoveryCleanupTx(tx, id, driver)
		if err != nil {
			return err
		}
		if !r.CopyRemovedAt.IsZero() {
			return nil
		}
		r.CopyRemovedAt = s.now().UTC()
		return saveWorkspaceRecoveryTx(tx, &r, "recovery-copy-removed")
	})
}

// RecordRecoveryWait is a bounded diagnostic, not proof, authorization or a
// replacement transaction state. It never changes the head or release fact.
func (s *Service) RecordRecoveryWait(ctx context.Context, id string, driver ledger.Lease, cause error) error {
	message := ""
	if cause != nil {
		message = strings.ToValidUTF8(cause.Error(), "�")
		if len(message) > 2048 {
			cut := 2048
			for !utf8.RuneStart(message[cut]) {
				cut--
			}
			message = message[:cut]
		}
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		if driver.Key != "workspace-recovery-driver:"+id {
			return ledger.ErrStale
		}
		if err := tx.CheckLocalLease(driver); err != nil {
			return err
		}
		r, err := recoveryByIDTx(tx, id)
		if err != nil {
			return err
		}
		if r.Error == message {
			return nil
		}
		r.Error = message
		return saveWorkspaceRecoveryTx(tx, &r, "recovery-wait")
	})
}

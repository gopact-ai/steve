package attempt

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

// RecoveryResidual is an accepted non-flattening snapshot of the fixed original
// target, not a full filesystem backup or a request to restore its baseline.
type RecoveryResidual struct {
	Artifact string `json:"artifact"`
	RecoveryContent
	CapturedAt     time.Time `json:"captured_at"`
	ExcludedNested []string  `json:"excluded_nested,omitempty"`
}

func (s *Service) FreezeRecovery(ctx context.Context, id string, driver ledger.Lease) (WorkspaceRecovery, error) {
	var result WorkspaceRecovery
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryDriverTx(tx, id, driver)
		if err != nil {
			return err
		}
		if r.Phase == "capture" || r.Phase == "landing" {
			result = r
			return nil
		}
		if r.Phase != "draining" || r.Producer != nil {
			return ErrWorkspaceRecovery
		}
		if err := recoveryConvergedTx(tx, r); err != nil {
			return err
		}
		r.FrozenHeadVersion, r.Phase = r.Head.Version, "capture"
		if err := saveWorkspaceRecoveryTx(tx, &r, "recovery-head-frozen"); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

func recoveryConvergedTx(tx *ledger.Tx, r WorkspaceRecovery) error {
	if r.Producer != nil {
		return ErrWorkspaceRecovery
	}
	if err := originalRecoveryStoppedTx(tx, r); err != nil {
		return err
	}
	for _, copy := range []bool{false, true} {
		if err := checkRecoveryNativeConvergenceTx(tx, r, copy); err != nil {
			return err
		}
	}
	return nil
}

// CheckRecoveryCaptureTx is the business permit consumed by artifact's exact
// residual acceptance. A holder string, source token or SHA is not a permit.
func CheckRecoveryCaptureTx(tx *ledger.Tx, id string, version int64, driver, canonical ledger.Lease) (WorkspaceRecovery, error) {
	r, err := recoveryByIDTx(tx, id)
	if err != nil {
		return r, err
	}
	if driver.Key != "workspace-recovery-driver:"+id || canonical.Key != "canonical:"+r.Project {
		return r, ledger.ErrStale
	}
	if err := tx.CheckLocalLease(driver); err != nil {
		return r, err
	}
	if canonical.Region == "" || canonical.Region == driver.Region {
		if err := tx.CheckLocalLease(canonical); err != nil {
			return r, err
		}
	}
	if r.Phase != "capture" || r.Residual != nil || r.FrozenHeadVersion != version || version != r.Head.Version {
		return r, ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, r); err != nil {
		return r, err
	}
	return r, recoveryConvergedTx(tx, r)
}

// AcceptRecoveryResidualTx is called after artifact/content/storage facts have
// been accepted in this same transaction. Rejected I/O never becomes evidence.
func AcceptRecoveryResidualTx(tx *ledger.Tx, id string, version int64, driver, canonical ledger.Lease, residual RecoveryResidual) error {
	r, err := CheckRecoveryCaptureTx(tx, id, version, driver, canonical)
	if err != nil {
		return err
	}
	if residual.CapturedAt.IsZero() {
		return errors.New("recovery residual lacks its observation")
	}
	if err := validateRecoveryContent(residual.Artifact, residual.ID, residual.Storage); err != nil {
		return err
	}
	e, err := contentreplica.LookupGitStorageEvidence(tx, residual.Evidence)
	if err != nil {
		return err
	}
	if e.Project != r.Project || e.Artifact != residual.Artifact || e.ContentID != residual.ID || e.Storage != residual.Storage {
		return contentreplica.ErrIntegrity
	}
	r.Residual, r.Phase = &residual, "landing"
	return saveWorkspaceRecoveryTx(tx, &r, "recovery-residual")
}

// CheckRecoveryFreezeTx authorizes only the drained copy verification before
// freezing. The original residual still requires its separate canonical lease.
func CheckRecoveryFreezeTx(tx *ledger.Tx, id string, driver ledger.Lease) (WorkspaceRecovery, error) {
	r, err := recoveryByIDTx(tx, id)
	if err != nil {
		return r, err
	}
	if driver.Key != "workspace-recovery-driver:"+id {
		return r, ledger.ErrStale
	}
	if err := tx.CheckLocalLease(driver); err != nil {
		return r, err
	}
	if r.Phase != "draining" || r.Producer != nil {
		return r, ErrWorkspaceRecovery
	}
	if err := checkRecoveryDeclarationTx(tx, r); err != nil {
		return r, err
	}
	return r, recoveryConvergedTx(tx, r)
}

package attempt

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func RecoveryLandingID(r WorkspaceRecovery) string {
	return fmt.Sprintf("recovery-land:%s:%d", r.ID, r.FrozenHeadVersion)
}

// CheckRecoveryLandingTx grants only the frozen episode's original target.
// New applying and zero-path acceptance check each distinct producer token;
// replay of an admitted WAL preserves its original decision instead.
func CheckRecoveryLandingTx(tx *ledger.Tx, id string, version int64, driver ledger.Lease, canonical *ledger.Lease, newAdmission bool) (WorkspaceRecovery, error) {
	r, err := checkRecoveryDriverTx(tx, id, driver)
	if err != nil {
		return r, err
	}
	if r.Phase != "landing" || r.Residual == nil || r.Producer != nil || r.FrozenHeadVersion != version || r.Head.Version != version {
		return r, ErrWorkspaceRecovery
	}
	if canonical != nil {
		if canonical.Key != "canonical:"+r.Project {
			return r, ledger.ErrStale
		}
		if canonical.Region == "" || canonical.Region == driver.Region {
			if err := tx.CheckLocalLease(*canonical); err != nil {
				return r, err
			}
		}
	}
	if err := recoveryConvergedTx(tx, r); err != nil {
		return r, err
	}
	if newAdmission {
		for _, source := range r.Head.Sources {
			if err := task.CheckExecutionTx(tx, &source.Execution); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

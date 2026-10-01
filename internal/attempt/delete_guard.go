package attempt

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskDeletionTx retains authority needed by native stopping or its
// projection. The owner predicate excludes completed history from this check.
func CheckTaskDeletionTx(tx ledger.Reader, taskIDs []string) error {
	if err := checkIdentityRows(tx); err != nil {
		return err
	}
	ids, err := json.Marshal(taskIDs)
	if err != nil {
		return err
	}
	r, err := scanIdentityRecord(tx.QueryRow(`SELECT `+identityColumns+` FROM operations INDEXED BY operations_attempt_stop_candidates WHERE `+stopCandidatePredicate+` AND `+identityTask+` IN (SELECT value FROM json_each(?)) ORDER BY updated_at DESC LIMIT 1`, string(ids)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: task %s execution %s", task.ErrRetirementPending, r.TaskID, r.ID)
}

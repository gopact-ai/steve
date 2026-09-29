package attempt

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx refuses while an attempt of one of the tasks is in
// flight or has not settled its session. It reads those tasks' attempts
// through the identity index, so its cost follows them and not every
// attempt in the ledger; an attempt that cannot be read, of whichever task,
// refuses as every identity read does.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	if err := checkIdentityRows(tx); err != nil {
		return err
	}
	var tasks []string
	for id, asked := range ids {
		if asked {
			tasks = append(tasks, id)
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	slices.Sort(tasks)
	raw, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	rows, err := tx.Query(tasksByUpdateSQL, string(raw))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanIdentityRecord(rows)
		if err != nil {
			return err
		}
		switch record.State {
		case Bound, Failed, Expired, BindConflict, Superseded:
		default:
			return fmt.Errorf("%w: attempt %s (%s)", task.ErrCompleteBusy, record.ID, record.State)
		}
		if record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled {
			return fmt.Errorf("%w: attempt %s", task.ErrCompleteBusy, record.ID)
		}
	}
	return rows.Err()
}

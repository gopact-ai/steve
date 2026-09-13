package attempt

import (
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		record, err := decode(op)
		if err != nil {
			return err
		}
		if !ids[record.TaskID] {
			continue
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
	return nil
}

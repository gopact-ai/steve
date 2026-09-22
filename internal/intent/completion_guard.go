package intent

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx requires every effect of the task tree to be resolved
// in the transaction that closes it. Operation state outranks payload state.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	operations, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, operation := range operations {
		var effect Intent
		if err := json.Unmarshal(operation.Data, &effect); err != nil {
			return fmt.Errorf("decode completion intent %s: %w", operation.ID, err)
		}
		if ids[effect.TaskID] && operation.State != string(Succeeded) && operation.State != string(Failed) {
			return task.ErrCompleteAttention
		}
	}
	return nil
}

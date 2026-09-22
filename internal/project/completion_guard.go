package project

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx keeps proposed disclosures open until the owner's
// decision is durable in the transaction closing the task tree.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	operations, err := tx.Operations(kindDisclosureOp, "")
	if err != nil {
		return err
	}
	for _, operation := range operations {
		var disclosure DisclosureRequest
		if err := json.Unmarshal(operation.Data, &disclosure); err != nil {
			return fmt.Errorf("decode completion disclosure %s: %w", operation.ID, err)
		}
		if ids[disclosure.TaskID] && operation.State == DisclosureProposed {
			return task.ErrCompleteAttention
		}
	}
	return nil
}

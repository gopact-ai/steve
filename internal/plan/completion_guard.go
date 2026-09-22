package plan

import (
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// CheckTaskCompletionTx excludes planned work using the same durable snapshot
// in which the caller will close the task tree.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	raw, _, err := tx.LoadDocument("plans")
	if err != nil {
		return fmt.Errorf("read completion plans: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var saved data
	if err := json.Unmarshal(raw, &saved); err != nil {
		return fmt.Errorf("decode completion plans: %w", err)
	}
	for id := range ids {
		if saved.ByTask[id] != "" {
			return task.ErrCompleteRoot
		}
	}
	return nil
}

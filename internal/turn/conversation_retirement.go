package turn

import (
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// The task store calls this again inside its deletion transaction. Neither
// owner opens another snapshot or reads a separately locked cache here.
func checkConversationRetirement(tx ledger.Reader, ids []string) error {
	if err := attempt.CheckTaskDeletionTx(tx, ids); err != nil {
		return err
	}
	if err := state.CheckTaskDeletionTx(tx, ids); err != nil {
		if errors.Is(err, state.ErrCloseOwed) {
			return fmt.Errorf("%w: %w", task.ErrRetirementPending, err)
		}
		return err
	}
	return exec.CheckTaskRunRetirementTx(tx, ids)
}

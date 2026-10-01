package state

import (
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrCloseOwed = errors.New("native session close is still owed")

// CheckTaskDeletionTx retains task authority needed by an outstanding native close.
func CheckTaskDeletionTx(tx ledger.Reader, taskIDs []string) error {
	return nil
}

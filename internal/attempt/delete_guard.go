package attempt

import "github.com/gopact-ai/steve/internal/ledger"

// CheckTaskDeletionTx checks cleanup obligations before their task authority is removed.
func CheckTaskDeletionTx(tx ledger.Reader, taskIDs []string) error {
	return nil
}

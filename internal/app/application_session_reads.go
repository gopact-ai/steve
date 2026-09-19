package app

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// Binding requires identity, not permission to start more work: cleanup must
// still bind the admitted session after its task's execution epoch is revoked.
func readSessionBinding(ctx context.Context, book *ledger.Ledger, key execution.Key, place harness.Placement, upstream, workdir string) (attempt.Record, task.Task, error) {
	var record attempt.Record
	var tracked task.Task
	err := book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		record, err = attempt.GetTx(tx, key.AttemptID)
		if err != nil {
			return err
		}
		if record.ID != key.AttemptID || record.TaskID != key.TaskID {
			return errors.New("node session does not match its admitted execution")
		}
		if err := validateSessionPlacement(record, place, upstream, workdir); err != nil {
			return err
		}
		if record.Execution == nil || record.Execution.TaskID != record.TaskID {
			return errors.New("node session requires a task execution token")
		}
		var found bool
		tracked, found, err = task.GetTx(tx, record.TaskID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("node session task is missing")
		}
		return nil
	})
	return record, tracked, err
}

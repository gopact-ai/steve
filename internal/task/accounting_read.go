package task

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const invalidAccountingReceipt = "!invalid"

func accountingReceiptKey(taskID, executionID, turnID string) string {
	raw, _ := json.Marshal([3]string{taskID, executionID, turnID})
	return string(raw)
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_task_accounting_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		raw, ok := args[0].(string)
		var row attemptRecord
		if !ok || json.Unmarshal([]byte(raw), &row) != nil || row.TaskID == "" || row.Index < 0 {
			return invalidAccountingReceipt, nil
		}
		return accountingReceiptKey(row.TaskID, row.ExecutionID, row.TurnID), nil
	})
	ledger.MustRegisterReadIndex("bindings_task_accounting_receipt",
		`CREATE INDEX IF NOT EXISTS bindings_task_accounting_receipt ON bindings(steve_task_accounting_v1(data)) WHERE kind='task-attempt'`)
}

// Include malformed candidates rather than silently treating them as absence.
// Two rows suffice to reject ambiguity without loading the task's history.
const accountingReceiptSQL = `SELECT count(*),min(id),min(data) FROM
	(SELECT id,data FROM bindings INDEXED BY bindings_task_accounting_receipt WHERE kind='task-attempt'
	AND steve_task_accounting_v1(data) IN (?, '!invalid') LIMIT 2)`

// ReadAccountingTx reads the single original accounting row in the caller's
// snapshot. It grants no deletion authority: the caller must check settlement,
// usage, result and delivery alongside this exact identity.
func ReadAccountingTx(tx ledger.Reader, taskID, executionID, turnID string) (Attempt, int, bool, error) {
	if taskID == "" || executionID == "" || turnID == "" {
		return Attempt{}, 0, false, errors.New("task accounting requires task, execution and turn identities")
	}
	var count int
	var id, raw sql.NullString
	if err := tx.QueryRow(accountingReceiptSQL, accountingReceiptKey(taskID, executionID, turnID)).Scan(&count, &id, &raw); err != nil {
		return Attempt{}, 0, false, err
	}
	if count == 0 {
		return Attempt{}, 0, false, nil
	}
	if count != 1 || !id.Valid || !raw.Valid {
		return Attempt{}, 0, false, errors.New("task accounting has ambiguous original execution")
	}
	var row attemptRecord
	if err := json.Unmarshal([]byte(raw.String), &row); err != nil {
		return Attempt{}, 0, false, fmt.Errorf("task accounting: %w", err)
	}
	if row.TaskID != taskID || row.ExecutionID != executionID || row.TurnID != turnID ||
		row.Index < 0 || id.String != attemptKey(taskID, row.Index) {
		return Attempt{}, 0, false, errors.New("task accounting identity differs from its owner record")
	}
	var head taskHead
	found, err := readRecordTx(tx, taskKind, taskID, &head)
	if err != nil {
		return Attempt{}, 0, false, err
	}
	if !found || head.ID != taskID || row.Index >= head.AttemptCount || len(head.Attempts) != 0 {
		return Attempt{}, 0, false, errors.New("task accounting has no matching task header")
	}
	return row.Attempt, row.Index, true, nil
}

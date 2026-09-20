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

const turnAccountingKey = `steve_task_turn_v1(id,data)`

func decodeTurnAccounting(id string, raw []byte) (attemptRecord, error) {
	var row *attemptRecord
	if err := json.Unmarshal(raw, &row); err != nil {
		return attemptRecord{}, err
	}
	if row == nil || row.TaskID == "" || row.Index < 0 || id != attemptKey(row.TaskID, row.Index) {
		return attemptRecord{}, errors.New("task: invalid turn accounting identity")
	}
	return *row, nil
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_task_turn_v1", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		var raw []byte
		switch value := args[1].(type) {
		case string:
			raw = []byte(value)
		case []byte:
			raw = value
		}
		row, err := decodeTurnAccounting(id, raw)
		if err != nil {
			return nil, nil
		}
		return row.TurnID, nil
	})
	ledger.MustRegisterReadIndex("bindings_task_turn", `CREATE INDEX IF NOT EXISTS bindings_task_turn ON bindings(`+turnAccountingKey+`) WHERE kind='task-attempt'`)
}

// ReadTurnAccountingTx reads one exact input's accounting and owner header in
// the caller's snapshot. Missing, legacy unnamed and ambiguous rows cannot
// prove a turn ended. Invalid rows remain visible in the index and fail closed.
func ReadTurnAccountingTx(tx ledger.Reader, turnID string) (Task, Attempt, bool, error) {
	if turnID == "" {
		return Task{}, Attempt{}, false, errors.New("task: exact turn identity is required")
	}
	var count int
	var id, raw sql.NullString
	err := tx.QueryRow(`SELECT count(*),min(id),min(data) FROM (
		SELECT id,data FROM bindings INDEXED BY bindings_task_turn WHERE kind='task-attempt'
		AND (`+turnAccountingKey+`=? OR `+turnAccountingKey+` IS NULL) LIMIT 2)`, turnID).Scan(&count, &id, &raw)
	if err != nil || count == 0 {
		return Task{}, Attempt{}, false, err
	}
	if count != 1 || !id.Valid || !raw.Valid {
		return Task{}, Attempt{}, false, errors.New("task: ambiguous turn accounting")
	}
	row, err := decodeTurnAccounting(id.String, []byte(raw.String))
	if err != nil {
		return Task{}, Attempt{}, false, err
	}
	var headerRaw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='task' AND id=?`, row.TaskID).Scan(&headerRaw); err != nil {
		return Task{}, Attempt{}, false, err
	}
	head, err := decodeScopeHeader(row.TaskID, []byte(headerRaw))
	if err != nil {
		return Task{}, Attempt{}, false, err
	}
	if row.TurnID != turnID || row.Index >= head.AttemptCount {
		return Task{}, Attempt{}, false, errors.New("task: turn accounting differs from its owner")
	}
	var control recordControl
	found, err := readRecordTx(tx, taskStoreKind, taskStoreID, &control)
	if err != nil {
		return Task{}, Attempt{}, false, err
	}
	if !found || control.Revision == 0 || control.NextID < 1 {
		return Task{}, Attempt{}, false, errors.New("task: turn accounting has no valid store control")
	}
	return head.Task, row.Attempt, true, nil
}

// CheckChatAdmissionTx fences delayed attempt creation using the accounting
// row, in the same transaction that creates the attempt. Closing an input must
// not revoke the whole task epoch: a later, distinct input may still run.
func CheckChatAdmissionTx(tx ledger.Reader, taskID, turnID string, token *ExecutionToken) error {
	if token == nil {
		if turnID != "" {
			_, _, found, err := ReadTurnAccountingTx(tx, turnID)
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("%w: accounted chat input requires its execution token", ErrExecutionStopped)
			}
		}
		var head taskHead
		found, err := readRecordTx(tx, taskKind, taskID, &head)
		if err != nil {
			return err
		}
		if found && (head.ID != taskID || head.AttemptCount < 0 || len(head.Attempts) != 0) {
			return fmt.Errorf("%w: chat accounting owner is invalid", ErrExecutionStopped)
		}
		if found && head.AttemptCount > 0 {
			return fmt.Errorf("%w: accounted task chat requires its execution token", ErrExecutionStopped)
		}
		return nil
	}
	if token.TaskID != taskID {
		return fmt.Errorf("%w: chat execution owner differs", ErrExecutionStopped)
	}
	if turnID != "" {
		tracked, row, found, err := ReadTurnAccountingTx(tx, turnID)
		if err != nil {
			return err
		}
		if found {
			if tracked.ID != token.TaskID || row.ExecutionEpoch != token.Epoch || row.Independent || !row.Open() || row.ExecutionID != "" {
				return fmt.Errorf("%w: chat input %s already ended or belongs to another execution", ErrExecutionStopped, turnID)
			}
			return nil
		}
	}
	// Existing unlabelled accounting can still be bound by its live owner.
	// It never supplies a never-admitted proof. In particular, no closed row
	// can be reopened merely because it predates durable input identities.
	var head taskHead
	found, err := readRecordTx(tx, taskKind, token.TaskID, &head)
	if err != nil {
		return err
	}
	if !found || head.ID != token.TaskID || head.AttemptCount < 0 || len(head.Attempts) != 0 {
		return fmt.Errorf("%w: chat accounting owner is missing", ErrExecutionStopped)
	}
	for index := head.AttemptCount - 1; index >= 0; index-- {
		var raw string
		key := attemptKey(token.TaskID, index)
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='task-attempt' AND id=?`, key).Scan(&raw); err != nil {
			return err
		}
		row, err := decodeTurnAccounting(key, []byte(raw))
		if err != nil {
			return err
		}
		if row.Independent {
			continue
		}
		if !row.Open() || row.ExecutionEpoch != token.Epoch || row.ExecutionID != "" || row.TurnID != "" ||
			(head.AnchorMessage != "" && head.AnchorMessage != turnID) {
			return fmt.Errorf("%w: chat accounting is not an open input", ErrExecutionStopped)
		}
		return nil
	}
	// Direct execution consumers may create a chat without task turn
	// accounting. They cannot produce the positive evidence recovery needs.
	return nil
}

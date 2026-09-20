package task

import (
	"errors"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

// ReadCancelledTurnTx reads the complete accounting of the unique cancelled
// root anchored to this input. It does not prove absence of native execution:
// the attempt owner must reconcile every historical execution in this same
// snapshot before using an old unlabelled final row.
func ReadCancelledTurnTx(tx *ledger.ReadTx, address channel.Address) (Task, bool, error) {
	ids, err := ConversationIDsTx(tx, address.Channel, address.Conversation)
	if err != nil {
		return Task{}, false, err
	}
	var selected Task
	var heads []Task
	for _, id := range ids {
		tracked, found, err := GetTx(tx, id)
		if err != nil || !found {
			return Task{}, false, errors.Join(errors.New("task: cancelled input owner is missing"), err)
		}
		heads = append(heads, tracked)
		if tracked.Transport == address.Channel && tracked.Channel == address.Conversation && tracked.AnchorMessage == address.Message {
			if selected.ID != "" {
				return Task{}, false, errors.New("task: cancelled input anchor is ambiguous")
			}
			selected = tracked
		}
	}
	if selected.ID == "" || selected.State != StateCancelled || selected.Parent != "" ||
		selected.PreparedPlan != nil || selected.RecoveryWorkspace != nil {
		return Task{}, false, nil
	}
	for _, tracked := range heads {
		if tracked.Parent == selected.ID {
			return Task{}, false, nil
		}
	}
	var head taskHead
	if found, err := readRecordTx(tx, taskKind, selected.ID, &head); err != nil || !found {
		return Task{}, false, errors.Join(errors.New("task: cancelled input header is missing"), err)
	}
	var control recordControl
	if found, err := readRecordTx(tx, taskStoreKind, taskStoreID, &control); err != nil || !found || control.Revision == 0 || control.NextID < 1 {
		return Task{}, false, errors.Join(errors.New("task: cancelled input store control is invalid"), err)
	}
	prefix := attemptPrefix(selected.ID)
	rows, err := tx.Query(`SELECT id,data FROM bindings WHERE kind='task-attempt' AND id>=? AND id<? ORDER BY id`, prefix, prefix+"~")
	if err != nil {
		return Task{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return Task{}, false, err
		}
		row, err := decodeTurnAccounting(id, raw)
		if err != nil {
			return Task{}, false, err
		}
		if row.TaskID != selected.ID || row.Index != len(selected.Attempts) {
			return Task{}, false, errors.New("task: cancelled input accounting is incomplete")
		}
		selected.Attempts = append(selected.Attempts, row.Attempt)
	}
	if err := rows.Err(); err != nil {
		return Task{}, false, err
	}
	if len(selected.Attempts) != head.AttemptCount {
		return Task{}, false, errors.New("task: cancelled input accounting count differs")
	}
	return selected, true, nil
}

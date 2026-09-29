package intent

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"modernc.org/sqlite"
)

const (
	completionTask  = `steve_intent_task_v1(data)`
	tasksIntentsSQL = `SELECT id,state,data FROM operations INDEXED BY operations_intent_task
		WHERE kind='intent' AND ` + completionTask + ` IN (SELECT value FROM json_each(?)) ORDER BY updated_at DESC`
	unreadableIntentSQL = `SELECT id,data FROM operations INDEXED BY operations_intent_task
		WHERE kind='intent' AND ` + completionTask + ` IS NULL LIMIT 1`
)

// The task key is what the guard's own decoder reads. An intent it cannot
// decode takes the NULL key, so the guard finds one without reading the rest.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_intent_task_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var raw []byte
		switch v := args[0].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		}
		var effect Intent
		if err := json.Unmarshal(raw, &effect); err != nil {
			return nil, nil
		}
		return effect.TaskID, nil
	})
	ledger.MustRegisterReadIndex("operations_intent_task", `CREATE INDEX IF NOT EXISTS operations_intent_task ON operations(`+completionTask+`,id) WHERE kind='intent'`)
}

// CheckTaskCompletionTx requires every effect of the task tree to be resolved
// in the transaction that closes it. Operation state outranks payload state.
// It reads those tasks' intents through their task key, so its cost follows
// them and not every intent in the ledger; an intent that cannot be read, of
// whichever task, refuses first.
func CheckTaskCompletionTx(tx *ledger.Tx, ids map[string]bool) error {
	if err := ledger.CheckOperationEnvelopesTx(tx, kind); err != nil {
		return err
	}
	var id, data string
	switch err := tx.QueryRow(unreadableIntentSQL).Scan(&id, &data); {
	case err == nil:
		var effect Intent
		return fmt.Errorf("decode completion intent %s: %w", id, json.Unmarshal([]byte(data), &effect))
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var tasks []string
	for id, asked := range ids {
		if asked {
			tasks = append(tasks, id)
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	slices.Sort(tasks)
	raw, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	rows, err := tx.Query(tasksIntentsSQL, string(raw))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		if err := rows.Scan(&id, &state, &data); err != nil {
			return err
		}
		var effect Intent
		if err := json.Unmarshal([]byte(data), &effect); err != nil {
			return fmt.Errorf("decode completion intent %s: %w", id, err)
		}
		if ids[effect.TaskID] && state != string(Succeeded) && state != string(Failed) {
			return task.ErrCompleteAttention
		}
	}
	return rows.Err()
}

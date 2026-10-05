package artifact

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const completionLandingKeySQL = `steve_landing_completion_key_v1(data)`
const unwrittenCompletionLandingSQL = `steve_landing_unwritten_v1(data,state)`
const standingTaskLandingsSQL = `SELECT candidate.state,candidate.data
	FROM operations AS candidate INDEXED BY operations_uncommitted_landings
	WHERE candidate.kind='landing' AND candidate.state<>'committed'
	AND steve_landing_task_v1(candidate.data)<>''
	AND NOT (steve_landing_unwritten_v1(candidate.data,candidate.state)=1 AND EXISTS(
		SELECT 1 FROM operations AS accepted INDEXED BY operations_landing_completion_key
		WHERE accepted.kind='landing' AND accepted.state='committed'
		AND steve_landing_completion_key_v1(accepted.data)=steve_landing_completion_key_v1(candidate.data)))
	ORDER BY candidate.id`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_landing_completion_key_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var land Landing
		if json.Unmarshal(completionRaw(args[0]), &land) != nil || land.Source == nil || land.Source.Execution == nil || land.Artifact == "" || land.Source.AttemptID == "" {
			return nil, nil
		}
		raw, err := json.Marshal(completionLandingIdentity(land))
		return string(raw), err
	})
	sqlite.MustRegisterDeterministicScalarFunction("steve_landing_unwritten_v1", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var land Landing
		if json.Unmarshal(completionRaw(args[0]), &land) != nil {
			return int64(0), nil
		}
		land.State = string(completionRaw(args[1]))
		if unwrittenCompletionLanding(land) {
			return int64(1), nil
		}
		return int64(0), nil
	})
	// Cache both typed predicates in local indexes. Otherwise even a SQL-side
	// exclusion would decode every retained, already-superseded refusal.
	ledger.MustRegisterReadIndex("operations_uncommitted_landings", `CREATE INDEX IF NOT EXISTS operations_uncommitted_landings ON operations(`+unwrittenCompletionLandingSQL+`,`+completionLandingKeySQL+`,id)
		WHERE kind='landing' AND state<>'committed' AND `+completionLandingTask+`<>''`)
	ledger.MustRegisterReadIndex("operations_landing_completion_key", `CREATE INDEX IF NOT EXISTS operations_landing_completion_key ON operations(`+completionLandingKeySQL+`)
		WHERE kind='landing' AND state='committed'`)
}

func completionRaw(value driver.Value) []byte {
	switch value := value.(type) {
	case string:
		return []byte(value)
	case []byte:
		return value
	default:
		return nil
	}
}

// CompletionBlockers reports the tasks whose results are still queued or have
// an unresolved landing. It reads standing work, not a recent history window;
// a hidden historical descendant must still keep its root from completing.
// Unknown records fail the read, just as they fail the transactional guard.
func (s *Store) CompletionBlockers(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.ledger.Read(ctx, func(tx *ledger.ReadTx) error {
		blocked, err := queuedCompletionTasks(tx)
		if err != nil {
			return err
		}
		if err := checkCompletionLandingRecords(tx); err != nil {
			return err
		}
		lands, err := standingTaskLandings(tx)
		if err != nil {
			return err
		}
		for _, land := range lands {
			blocked[land.Source.Execution.TaskID] = true
		}
		for id := range blocked {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return nil
	})
	return ids, err
}

func queuedCompletionTasks(tx *ledger.ReadTx) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT id,data FROM bindings WHERE kind=? ORDER BY id`, pendingKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blocked := map[string]bool{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var pending Pending
		if err := json.Unmarshal(raw, &pending); err != nil {
			return nil, fmt.Errorf("read pending landing %s: %w", id, err)
		}
		if pending.Source != nil && pending.Source.Execution != nil && pending.Source.Execution.TaskID != "" {
			blocked[pending.Source.Execution.TaskID] = true
		}
	}
	return blocked, rows.Err()
}

func standingTaskLandings(tx *ledger.ReadTx) ([]Landing, error) {
	rows, err := tx.Query(standingTaskLandingsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Landing
	for rows.Next() {
		var state string
		var raw []byte
		if err := rows.Scan(&state, &raw); err != nil {
			return nil, err
		}
		var land Landing
		if err := json.Unmarshal(raw, &land); err != nil {
			return nil, err
		}
		if land.Source == nil || land.Source.Execution == nil || land.Source.Execution.TaskID == "" {
			continue // A result produced without a task has no task to close.
		}
		land.State = state
		out = append(out, land)
	}
	return out, rows.Err()
}

func checkCompletionLandingRecords(tx ledger.Reader) error {
	if err := ledger.CheckOperationEnvelopesTx(tx, landKind); err != nil {
		return err
	}
	var data string
	switch err := tx.QueryRow(unreadableLandingSQL).Scan(&data); {
	case err == nil:
		var land Landing
		return json.Unmarshal([]byte(data), &land)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return nil
}

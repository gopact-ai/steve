package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

const standingTaskLandingsSQL = `SELECT state,data FROM operations INDEXED BY operations_uncommitted_landings
	WHERE kind='landing' AND state<>'committed' ORDER BY id`

func init() {
	ledger.MustRegisterReadIndex("operations_uncommitted_landings", `CREATE INDEX IF NOT EXISTS operations_uncommitted_landings ON operations(id) WHERE kind='landing' AND state<>'committed'`)
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
		committed, err := committedCompletionLandings(tx, lands)
		if err != nil {
			return err
		}
		for _, land := range lands {
			if !landingWasSuperseded(land, committed) {
				blocked[land.Source.Execution.TaskID] = true
			}
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

func committedCompletionLandings(tx *ledger.ReadTx, lands []Landing) (map[completionLandingKey]bool, error) {
	keys := map[completionLandingKey]bool{}
	tasks := map[string]bool{}
	for _, land := range lands {
		if unwrittenCompletionLanding(land) {
			tasks[land.Source.Execution.TaskID] = true
		}
	}
	if len(tasks) == 0 {
		return keys, nil
	}
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT data FROM operations INDEXED BY operations_landing_task
		WHERE kind='landing' AND `+completionLandingTask+` IN (SELECT value FROM json_each(?)) AND state='committed'`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var land Landing
		if err := json.Unmarshal(raw, &land); err != nil {
			return nil, err
		}
		if land.Source != nil && land.Source.Execution != nil && land.Artifact != "" && land.Source.AttemptID != "" {
			keys[completionLandingIdentity(land)] = true
		}
	}
	return keys, rows.Err()
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

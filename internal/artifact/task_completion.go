package artifact

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"modernc.org/sqlite"
)

const (
	completionLandingTask = `steve_landing_task_v1(data)`
	tasksLandingsSQL      = `SELECT state,data FROM operations INDEXED BY operations_landing_task
		WHERE kind='landing' AND ` + completionLandingTask + ` IN (SELECT value FROM json_each(?)) ORDER BY updated_at DESC`
	unreadableLandingSQL = `SELECT data FROM operations INDEXED BY operations_landing_task
		WHERE kind='landing' AND ` + completionLandingTask + ` IS NULL LIMIT 1`
)

// The task key is what the completion check's own decoder reads: the task
// whose execution the landing's result came from, empty without one. A
// landing it cannot decode takes the NULL key, so the check finds one
// without reading the rest.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_landing_task_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		var raw []byte
		switch v := args[0].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		}
		var land Landing
		if err := json.Unmarshal(raw, &land); err != nil {
			return nil, nil
		}
		if land.Source == nil || land.Source.Execution == nil {
			return "", nil
		}
		return land.Source.Execution.TaskID, nil
	})
	ledger.MustRegisterReadIndex("operations_landing_task", `CREATE INDEX IF NOT EXISTS operations_landing_task ON operations(`+completionLandingTask+`,id) WHERE kind='landing'`)
}

type completionLandingKey struct {
	Project, Artifact, Attempt string
	Target                     project.Home
	Execution                  task.ExecutionToken
}

func completionLandingIdentity(land Landing) completionLandingKey {
	return completionLandingKey{Project: land.Project, Artifact: land.Artifact, Attempt: land.Source.AttemptID, Target: land.Target, Execution: *land.Source.Execution}
}

// CheckTaskLandingsTx retains real conflicts and unfinished WALs. Older queue
// drainers could race, leaving a terminal lock refusal alongside a successful
// landing of the exact same result. That refusal never acquired a write lease;
// its historical record remains intact without blocking accepted work forever.
// Unapplied is explicit preapply-stop proof; snapshots and candidate merge paths
// remain evidence even when no canonical write was admitted. Landings are read
// through their task key, so the cost follows the tasks' landings and not
// every landing in the ledger; a landing that cannot be read, of whichever
// task, refuses first.
func CheckTaskLandingsTx(tx *ledger.Tx, ids map[string]bool) error {
	queue, err := tx.Bindings(pendingKind)
	if err != nil {
		return err
	}
	for _, raw := range queue {
		var pending Pending
		if err := json.Unmarshal(raw, &pending); err != nil {
			return err
		}
		if pending.Source != nil && pending.Source.Execution != nil && ids[pending.Source.Execution.TaskID] {
			return fmt.Errorf("%w: artifact %s is queued to land", task.ErrCompleteDelivery, pending.Artifact)
		}
	}
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
	rows, err := tx.Query(tasksLandingsSQL, string(raw))
	if err != nil {
		return err
	}
	defer rows.Close()
	var landings []Landing
	committed := map[completionLandingKey]bool{}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state, &data); err != nil {
			return err
		}
		var land Landing
		if err := json.Unmarshal([]byte(data), &land); err != nil {
			return err
		}
		if land.Source == nil || land.Source.Execution == nil || !ids[land.Source.Execution.TaskID] {
			continue
		}
		land.State = state
		landings = append(landings, land)
		if land.State == LandCommitted && land.Artifact != "" && land.Source.AttemptID != "" {
			committed[completionLandingIdentity(land)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, land := range landings {
		if land.State == LandCommitted {
			continue
		}
		unwritten := land.State == LandMergeConflicted && land.Lease == nil && land.Round == 0 && !land.EndedAt.IsZero() && (land.Unapplied || (land.Now == "" && land.Merged == "" && len(land.Paths) == 0))
		if unwritten && committed[completionLandingIdentity(land)] {
			continue
		}
		return fmt.Errorf("%w: landing %s (%s)", task.ErrCompleteDelivery, land.ID, land.State)
	}
	return nil
}

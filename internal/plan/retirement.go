package plan

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

// RetirementPlansTx reads plan ownership in the caller's snapshot. Only
// possibly related plans are validated here; unrelated histories are retained
// for the run owner's cross-check without acquiring execution authority.
func RetirementPlansTx(tx ledger.Reader, taskIDs []string) (map[string]Plan, error) {
	selected := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		selected[id] = true
	}
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='document' AND id='plans'`).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if trimmed := bytes.TrimSpace([]byte(raw)); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("retiring plans owner state must be an object")
	}
	var saved data
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return nil, fmt.Errorf("read retiring plans: %w", err)
	}
	related := map[string]bool{}
	for taskID, id := range saved.ByTask {
		if !selected[taskID] {
			continue
		}
		p, found := latest(saved, id)
		if !found || p.TaskID != taskID {
			return nil, fmt.Errorf("invalid retiring plan binding for task %s", taskID)
		}
		related[id] = true
	}
	plans := make(map[string]Plan, len(saved.Plans))
	for id, revisions := range saved.Plans {
		if len(revisions) == 0 {
			if related[id] {
				return nil, fmt.Errorf("retiring plan %s has no history", id)
			}
			continue
		}
		p := revisions[len(revisions)-1]
		if related[id] || selected[p.TaskID] || p.Execution != nil && selected[p.Execution.TaskID] {
			if p.ID != id || !selected[p.TaskID] || p.Execution != nil && p.Execution.TaskID != p.TaskID {
				return nil, fmt.Errorf("invalid retiring plan ownership %s", id)
			}
		}
		plans[id] = p
	}
	return plans, nil
}

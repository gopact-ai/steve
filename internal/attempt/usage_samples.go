package attempt

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// UsageSample is what spend accounting reads from one settled attempt.
type UsageSample struct {
	TaskID    string    `json:"task_id"`
	Project   string    `json:"project"`
	Harness   string    `json:"harness"`
	Agent     string    `json:"agent"`
	Usage     *Usage    `json:"usage,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// UsageSample is the record's spend accounting fields.
func (r Record) UsageSample() UsageSample {
	return UsageSample{TaskID: r.TaskID, Project: r.Project, Harness: r.Harness, Agent: r.Agent, Usage: r.Usage, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
}

const (
	usageColumns  = `id,state,updated_at,data`
	usageAllSQL   = `SELECT ` + usageColumns + ` FROM operations WHERE kind='attempt'`
	usageTasksSQL = `SELECT ` + usageColumns + ` FROM operations INDEXED BY operations_attempt_task
		WHERE kind='attempt' AND ` + identityTask + ` IN (SELECT value FROM json_each(?))`
	usageRevisionsSQL = `SELECT id,data FROM bindings WHERE kind='` + historyRevisionKind + `'`
)

type usageRow struct {
	updated, id string
	sample      UsageSample
}

type usageScope struct {
	revision string
	rows     []usageRow
}

// usageCache keeps each task's settled samples under the read revision they
// were read at. The revision moves with every write to the task's attempts,
// so a later read decodes only the tasks whose revision moved.
type usageCache struct {
	mu         sync.Mutex
	generation uint64
	reusable   bool
	scopes     map[string]usageScope
	samples    []UsageSample
}

// UsageSamples lists every settled attempt's spend fields, most recently
// updated first. Each read still refuses any attempt row the owner decoder
// cannot read, but decodes again only the tasks whose history changed since
// the previous read. The result is shared between reads: do not modify it.
func (s *Service) UsageSamples(ctx context.Context) ([]UsageSample, error) {
	c := &s.usage
	c.mu.Lock()
	defer c.mu.Unlock()
	generation := s.l.RestoreGeneration()
	scopes := c.scopes
	if !c.reusable || generation != c.generation {
		scopes = nil
	}
	var changed map[string]usageScope
	var removed []string
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		if err := checkIdentityRows(tx); err != nil {
			return err
		}
		revisions, err := readUsageRevisions(tx)
		if err != nil {
			return err
		}
		for id := range scopes {
			if _, ok := revisions[id]; !ok {
				removed = append(removed, id)
			}
		}
		if scopes == nil {
			changed, err = readAllUsage(tx, revisions)
			return err
		}
		var ids []string
		for id, revision := range revisions {
			if scopes[id].revision != revision {
				ids = append(ids, id)
			}
		}
		changed, err = readTaskUsage(tx, revisions, ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	if scopes == nil {
		c.scopes, c.samples = make(map[string]usageScope, len(changed)), nil
	}
	if len(changed) > 0 || len(removed) > 0 || c.samples == nil {
		for _, id := range removed {
			delete(c.scopes, id)
		}
		for id, scope := range changed {
			c.scopes[id] = scope
		}
		c.samples = mergeUsage(c.scopes)
	}
	// A restore in progress, or one that began or ended during the read,
	// leaves nothing to reuse.
	c.generation = generation
	c.reusable = generation%2 == 0 && s.l.RestoreGeneration() == generation
	return c.samples, nil
}

func readUsageRevisions(tx *ledger.ReadTx) (map[string]string, error) {
	rows, err := tx.Query(usageRevisionsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, revision string
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, err
		}
		out[id] = revision
	}
	return out, rows.Err()
}

// readAllUsage reads every task's samples. Attempts filed under a task with
// no read revision are corrupt history, as they are for every other read.
func readAllUsage(tx *ledger.ReadTx, revisions map[string]string) (map[string]usageScope, error) {
	scopes, err := scanUsage(tx, revisions, usageAllSQL)
	if err != nil {
		return nil, err
	}
	for id := range scopes {
		if _, ok := revisions[id]; !ok {
			return nil, fmt.Errorf("attempt: task %q has history but no read revision", id)
		}
	}
	return scopes, nil
}

func readTaskUsage(tx *ledger.ReadTx, revisions map[string]string, ids []string) (map[string]usageScope, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	scopes, err := scanUsage(tx, revisions, usageTasksSQL, string(raw))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := scopes[id]; !ok {
			scopes[id] = usageScope{revision: revisions[id]}
		}
	}
	return scopes, nil
}

func scanUsage(tx *ledger.ReadTx, revisions map[string]string, query string, args ...any) (map[string]usageScope, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := map[string]usageScope{}
	for rows.Next() {
		var row usageRow
		var state, raw string
		if err := rows.Scan(&row.id, &state, &row.updated, &raw); err != nil {
			return nil, err
		}
		// checkIdentityRows has already proven every row decodes in full.
		if err := json.Unmarshal([]byte(raw), &row.sample); err != nil {
			return nil, fmt.Errorf("attempt %s: %w", row.id, err)
		}
		scope := scopes[row.sample.TaskID]
		scope.revision = revisions[row.sample.TaskID]
		if State(state).Terminal() {
			scope.rows = append(scope.rows, row)
		}
		scopes[row.sample.TaskID] = scope
	}
	return scopes, rows.Err()
}

func mergeUsage(scopes map[string]usageScope) []UsageSample {
	var rows []usageRow
	for _, scope := range scopes {
		rows = append(rows, scope.rows...)
	}
	slices.SortFunc(rows, func(a, b usageRow) int {
		if c := cmp.Compare(b.updated, a.updated); c != 0 {
			return c
		}
		return cmp.Compare(b.id, a.id)
	})
	out := make([]UsageSample, len(rows))
	for i, row := range rows {
		out[i] = row.sample
	}
	return out
}

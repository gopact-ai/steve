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
	usageCountSQL     = `SELECT count(*) FROM operations WHERE kind='attempt'`
)

type usageRow struct {
	updated, id string
	sample      UsageSample
}

type usageScope struct {
	revision string
	// attempts counts every attempt row filed under the task, settled or
	// not, so a row added or removed without moving a revision changes the
	// ledger's total and forces a full read.
	attempts int
	rows     []usageRow
}

// usageCache keeps each task's settled samples under the read revision they
// were read at. The revision moves with every write to the task's attempts,
// so a later read decodes only the tasks whose revision moved. Listing the
// revisions still visits one binding per task: a cached read is linear in
// the number of tasks, not in their attempts.
type usageCache struct {
	// mu serializes reads: one refresh at a time updates the cache, and
	// concurrent callers wait for it rather than decoding the same tasks.
	mu         sync.Mutex
	generation uint64
	reusable   bool
	scopes     map[string]usageScope
	attempts   int
	samples    []UsageSample
}

// usageAfterRead, when set, runs between a usage read and its restore
// check. It is nil outside tests.
var usageAfterRead func(*usageCache)

type usageRead struct {
	full   bool
	scopes map[string]usageScope
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
	full := !c.reusable || generation != c.generation
	read, err := s.readUsage(ctx, full)
	after := s.l.RestoreGeneration()
	if err == nil && !read.full && after != generation {
		// Cached tasks were compared against history a restore replaced.
		generation = after
		read, err = s.readUsage(ctx, true)
		after = s.l.RestoreGeneration()
	}
	if err != nil {
		return nil, err
	}
	c.apply(read)
	// A restore in progress, or one that began or ended during the read,
	// leaves nothing to reuse.
	c.generation = generation
	c.reusable = generation%2 == 0 && after == generation
	return c.samples, nil
}

// readUsage reads the tasks whose revision moved, or every task when full.
// A cached read that finds history no longer matching what it cached, such
// as a revision gone or a row filed without moving one, reads in full so it
// reports exactly what a first read would.
func (s *Service) readUsage(ctx context.Context, full bool) (usageRead, error) {
	c := &s.usage
	read := usageRead{full: full}
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		if err := checkIdentityRows(tx); err != nil {
			return err
		}
		revisions, err := readUsageRevisions(tx)
		if err != nil {
			return err
		}
		if !read.full {
			var current bool
			read.scopes, current, err = c.readChanged(tx, revisions)
			if err != nil {
				return err
			}
			read.full = !current
		}
		if read.full {
			read.scopes, err = readAllUsage(tx, revisions)
		}
		return err
	})
	if usageAfterRead != nil {
		usageAfterRead(c)
	}
	return read, err
}

// readChanged reads the tasks whose revision moved. It reports false when
// the cached tasks no longer account for the ledger's attempts: a cached
// task lost its revision, or the attempt count differs from the tasks'.
func (c *usageCache) readChanged(tx *ledger.ReadTx, revisions map[string]string) (map[string]usageScope, bool, error) {
	for id := range c.scopes {
		if _, ok := revisions[id]; !ok {
			return nil, false, nil
		}
	}
	var ids []string
	for id, revision := range revisions {
		if c.scopes[id].revision != revision {
			ids = append(ids, id)
		}
	}
	changed, err := readTaskUsage(tx, revisions, ids)
	if err != nil {
		return nil, false, err
	}
	attempts := c.attempts
	for id, scope := range changed {
		attempts += scope.attempts - c.scopes[id].attempts
	}
	var total int
	if err := tx.QueryRow(usageCountSQL).Scan(&total); err != nil {
		return nil, false, err
	}
	return changed, total == attempts, nil
}

func (c *usageCache) apply(read usageRead) {
	if read.full {
		c.scopes, c.attempts, c.samples = make(map[string]usageScope, len(read.scopes)), 0, nil
	}
	if len(read.scopes) == 0 && c.samples != nil {
		return
	}
	for id, scope := range read.scopes {
		c.attempts += scope.attempts - c.scopes[id].attempts
		c.scopes[id] = scope
	}
	c.samples = mergeUsage(c.scopes)
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
		scope.attempts++
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

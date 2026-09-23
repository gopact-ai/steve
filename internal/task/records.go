package task

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const (
	taskKind        = "task"
	taskAttemptKind = "task-attempt"
	taskMetaKind    = "task-meta"
	taskStoreKind   = "task-store"
	taskStoreID     = "state"
)

type recordControl struct {
	NextID   int    `json:"next_id"`
	Revision uint64 `json:"revision"`
}
type taskHead struct {
	Task
	AttemptCount int `json:"attempt_count"`
}
type attemptRecord struct {
	TaskID string `json:"task_id"`
	Index  int    `json:"index"`
	Attempt
}

func headOf(t *Task) taskHead {
	head := taskHead{Task: *t, AttemptCount: len(t.Attempts)}
	head.Attempts = nil
	return head
}
func attemptPrefix(id string) string         { return base64.RawURLEncoding.EncodeToString([]byte(id)) + "/" }
func attemptKey(id string, index int) string { return fmt.Sprintf("%s%020d", attemptPrefix(id), index) }

func readRecordTx(tx ledger.Reader, kind, id string, out any) (bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return true, fmt.Errorf("task: decode %s/%s: %w", kind, id, err)
	}
	return true, nil
}
func controlTx(tx *ledger.Tx) (recordControl, error) {
	control := recordControl{NextID: 1}
	found, err := readRecordTx(tx, taskStoreKind, taskStoreID, &control)
	if err != nil {
		return control, err
	}
	if found && (control.NextID < 1 || control.Revision == 0) {
		return control, errors.New("task: invalid store control record")
	}
	return control, nil
}

// GetTx reads only the task's header under the caller's transaction. It does
// not load historical Attempts. Complete task histories belong to Store.Get
// and the owner export API, not execution/grant admission checks.
func GetTx(tx ledger.Reader, id string) (Task, bool, error) {
	var head taskHead
	found, err := readRecordTx(tx, taskKind, id, &head)
	if err != nil || !found {
		return Task{}, found, err
	}
	if head.ID != id || head.AttemptCount < 0 || len(head.Attempts) != 0 {
		return Task{}, false, fmt.Errorf("task: invalid header %s", id)
	}
	return head.Task, true, nil
}

type recordSet map[string]map[string]json.RawMessage

func emptyRecords() recordSet {
	return recordSet{taskKind: {}, taskAttemptKind: {}, taskMetaKind: {}, taskStoreKind: {}}
}
func loadRecordSet(book *ledger.Ledger) (recordSet, error) {
	// One SQL statement gives all four kinds one committed read boundary,
	// without a write transaction or a coordinator-only replication Prepare.
	rows, err := book.DB().Query(`SELECT kind,id,data FROM bindings WHERE kind IN (?,?,?,?)`, taskKind, taskAttemptKind, taskMetaKind, taskStoreKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := emptyRecords()
	for rows.Next() {
		var kind, id string
		var raw []byte
		if err := rows.Scan(&kind, &id, &raw); err != nil {
			return nil, err
		}
		records[kind][id] = append(json.RawMessage(nil), raw...)
	}
	return records, rows.Err()
}
func loadRecordSetTx(tx *ledger.Tx) (recordSet, error) {
	records := emptyRecords()
	for kind := range records {
		values, err := tx.Bindings(kind)
		if err != nil {
			return nil, err
		}
		records[kind] = values
	}
	return records, nil
}
func decodeRecords(records recordSet) (data, uint64, error) {
	d := data{NextID: 1, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}
	control := recordControl{NextID: 1}
	raw, exists := records[taskStoreKind][taskStoreID]
	if exists {
		if err := json.Unmarshal(raw, &control); err != nil {
			return d, 0, fmt.Errorf("task: decode store control: %w", err)
		}
		if control.NextID < 1 || control.Revision == 0 {
			return d, 0, errors.New("task: invalid store control record")
		}
	} else if len(records[taskKind])+len(records[taskAttemptKind])+len(records[taskMetaKind]) != 0 {
		return d, 0, errors.New("task: records exist without store control")
	}
	d.NextID = control.NextID
	counts := map[string]int{}
	for id, raw := range records[taskKind] {
		var head taskHead
		if err := json.Unmarshal(raw, &head); err != nil {
			return d, 0, fmt.Errorf("task: decode header %s: %w", id, err)
		}
		if id == "" || head.ID != id || head.AttemptCount < 0 || head.AttemptCount > len(records[taskAttemptKind]) || len(head.Attempts) != 0 {
			return d, 0, fmt.Errorf("task: invalid header %s", id)
		}
		if head.AttemptCount > 0 {
			head.Attempts = make([]Attempt, head.AttemptCount)
		}
		d.Tasks[id] = &head.Task
		counts[id] = 0
	}
	for key, raw := range records[taskAttemptKind] {
		var row attemptRecord
		if err := json.Unmarshal(raw, &row); err != nil {
			return d, 0, fmt.Errorf("task: decode attempt %s: %w", key, err)
		}
		tracked := d.Tasks[row.TaskID]
		if tracked == nil || row.Index < 0 || row.Index >= len(tracked.Attempts) || key != attemptKey(row.TaskID, row.Index) {
			return d, 0, fmt.Errorf("task: orphan or invalid attempt %s", key)
		}
		tracked.Attempts[row.Index] = row.Attempt
		counts[row.TaskID]++
	}
	for id, tracked := range d.Tasks {
		if counts[id] != len(tracked.Attempts) {
			return d, 0, fmt.Errorf("task: incomplete attempt history %s", id)
		}
	}
	for id, raw := range records[taskMetaKind] {
		var meta Meta
		if d.Tasks[id] == nil {
			return d, 0, fmt.Errorf("task: orphan metadata %s", id)
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return d, 0, fmt.Errorf("task: decode metadata %s: %w", id, err)
		}
		d.Meta[id] = meta
	}
	return d, control.Revision, nil
}

// OpenLedger reads the task record bindings, the only task authority.
func OpenLedger(book *ledger.Ledger) (*Store, error) {
	records, err := loadRecordSet(book)
	if err != nil {
		return nil, fmt.Errorf("read task records: %w", err)
	}
	loaded, revision, err := decodeRecords(records)
	if err != nil {
		return nil, err
	}
	s := &Store{book: book, data: loaded, revision: revision, now: time.Now, maxTurns: DefaultMaxTurns, maxElapsed: DefaultMaxElapsed}
	s.rebuildReadIndexLocked()
	return s, nil
}

type recordChange struct {
	kind, id       string
	value          any
	removeAttempts bool
}

func recordChanges(before, next data) ([]recordChange, error) {
	var changes []recordChange
	for id, t := range next.Tasks {
		if t == nil || t.ID != id {
			return nil, fmt.Errorf("task: invalid task %s", id)
		}
		old := before.Tasks[id]
		if old == nil || !reflect.DeepEqual(headOf(old), headOf(t)) {
			changes = append(changes, recordChange{kind: taskKind, id: id, value: headOf(t)})
		}
		for i, row := range t.Attempts {
			if old == nil || i >= len(old.Attempts) || !reflect.DeepEqual(old.Attempts[i], row) {
				changes = append(changes, recordChange{kind: taskAttemptKind, id: attemptKey(id, i), value: attemptRecord{TaskID: id, Index: i, Attempt: row}})
			}
		}
		if old != nil && len(old.Attempts) > len(t.Attempts) {
			return nil, errors.New("task: attempt history cannot be truncated")
		}
	}
	for id := range before.Tasks {
		if next.Tasks[id] == nil {
			changes = append(changes, recordChange{kind: taskKind, id: id}, recordChange{kind: taskAttemptKind, id: attemptPrefix(id), removeAttempts: true})
		}
	}
	for id, meta := range next.Meta {
		if next.Tasks[id] == nil {
			return nil, fmt.Errorf("task: orphan metadata %s", id)
		}
		old, found := before.Meta[id]
		if !found || !meta.equal(old) {
			changes = append(changes, recordChange{kind: taskMetaKind, id: id, value: meta})
		}
	}
	for id := range before.Meta {
		if _, ok := next.Meta[id]; !ok {
			changes = append(changes, recordChange{kind: taskMetaKind, id: id})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].kind != changes[j].kind {
			return changes[i].kind < changes[j].kind
		}
		return changes[i].id < changes[j].id
	})
	return changes, nil
}
func writeRecordChangesTx(tx *ledger.Tx, changes []recordChange, nextID int, revision uint64) error {
	for _, change := range changes {
		var err error
		switch {
		case change.removeAttempts:
			_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id>=? AND id<?`, change.kind, change.id, change.id+":")
		case change.value == nil:
			_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, change.kind, change.id)
		default:
			err = tx.PutBinding(change.kind, change.id, change.value)
		}
		if err != nil {
			return fmt.Errorf("task: write %s/%s: %w", change.kind, change.id, err)
		}
	}
	if revision == ^uint64(0) {
		return errors.New("task: store revision exhausted")
	}
	return tx.PutBinding(taskStoreKind, taskStoreID, recordControl{NextID: nextID, Revision: revision + 1})
}
func (s *Store) replaceRecordsLocked(ctx context.Context, next data, guard func(*ledger.Tx) error) error {
	changes, err := recordChanges(s.data, next)
	if err != nil {
		return err
	}
	changed := len(changes) > 0 || next.NextID != s.data.NextID
	err = s.book.Update(ctx, func(tx *ledger.Tx) error {
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		control, err := controlTx(tx)
		if err != nil {
			return err
		}
		if control.Revision != s.revision || control.NextID != s.data.NextID {
			return ledger.ErrConflict
		}
		if !changed {
			return nil
		}
		return writeRecordChangesTx(tx, changes, next.NextID, s.revision)
	})
	if err != nil {
		return err
	}
	if changed {
		s.revision++
	}
	s.installLocked(next, changes)
	return nil
}

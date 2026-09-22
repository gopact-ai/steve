package task

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type countedHeaderReader struct {
	ledger.Reader
	queries int
}

func (r *countedHeaderReader) QueryRow(query string, args ...any) *ledger.Row {
	r.queries++
	return r.Reader.QueryRow(query, args...)
}

func TestHeaderReadAndAuthorizationAreBoundedByLineageNotHistory(t *testing.T) {
	s, book := taskRecordBook(t)
	root, err := s.Create(Task{Member: "root", Channel: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.ExecutionToken(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	check := func() float64 {
		t.Helper()
		return testing.AllocsPerRun(10, func() {
			err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				r := &countedHeaderReader{Reader: tx}
				got, found, err := GetTx(r, child.ID)
				if err != nil || !found || got.ID != child.ID || len(got.Attempts) != 0 || r.queries != 1 {
					t.Fatalf("unbounded/invalid header: %+v found=%v queries=%d err=%v", got, found, r.queries, err)
				}
				if err := CheckExecutionTx(r, &token); err != nil {
					return err
				}
				if r.queries != 3 {
					t.Fatalf("expected one header and two lineage lookups, got %d", r.queries)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	before := check()
	next := s.clone()
	at := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for i := range 10000 {
		row := Attempt{StartedAt: at, EndedAt: at.Add(time.Second), ExecutionID: fmt.Sprint(i), Outcome: OutcomeOK}
		next.Tasks[child.ID].Attempts = append(next.Tasks[child.ID].Attempts, row)
		id := fmt.Sprint(i + 3)
		next.Tasks[id] = &Task{ID: id, State: StateDone, ExecutionEpoch: 1, Attempts: []Attempt{row}, CreatedAt: at, UpdatedAt: at}
	}
	next.NextID = 10003
	if err := s.replaceLocked(next); err != nil {
		t.Fatal(err)
	}
	after := check()
	t.Logf("10k closed tasks + 10k own rows: allocations %.0f -> %.0f; queries=3", before, after)
	if after > before+20 {
		t.Fatalf("header allocations grew with history: %.0f -> %.0f", before, after)
	}
	// Change only the ancestor, leaving the child's token untouched. A
	// header-only authorization must still walk the complete lineage.
	if _, err := s.Advance(root.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	err = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		return CheckExecutionTx(tx, &token)
	})
	if !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("ancestor stop ignored: %v", err)
	}
}

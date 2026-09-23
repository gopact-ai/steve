package app

import (
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestSessionBindingReadsOnlyItsCommittedAttemptAndTaskHeader(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:header-only", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := attempt.New(book).Open(t.Context(), attempt.Spec{ID: "read-binding", TaskID: tracked.ID, Agent: "worker", Node: "worker", Harness: "mock", Execution: &token, Scope: attempt.ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	// These are unrelated historical accounting rows. Opening the whole task
	// store now fails, but a session binding must not even decode them.
	if _, err := book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('task-attempt','unrelated','invalid','2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := task.OpenLedger(book); err == nil {
		t.Fatal("fixture did not distinguish header reads from full-store loads")
	}
	key := execution.Key{TaskID: tracked.ID, AttemptID: record.ID}
	place := harness.Placement{Node: "worker", Harness: "mock"}
	got, header, err := readSessionBinding(t.Context(), book, key, place, "", "")
	if err != nil || got.ID != record.ID || header.Channel != tracked.Channel || len(header.Attempts) != 0 {
		t.Fatalf("header binding: record=%+v header=%+v err=%v", got, header, err)
	}
	key.TaskID = "another-task"
	if _, _, err := readSessionBinding(t.Context(), book, key, place, "", ""); err == nil {
		t.Fatal("binding accepted a different task identity")
	}
}

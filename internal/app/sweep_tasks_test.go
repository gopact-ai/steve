package app

import (
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// A sweep that cannot read whether a quiet task still has an attempt in
// flight must leave the task alone: a failed read is not evidence of idleness.
func TestIdleSweepKeepsTasksWhoseLivenessCannotBeRead(t *testing.T) {
	output := captureLog(t)
	tasks, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := tasks.Create(task.Task{Goal: "quiet chat", Channel: "c1", Member: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(quiet.ID, "codex", "", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	unreadable, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	unreadable.Close()
	closeIdleTasks(t.Context(), tasks, attempt.New(unreadable), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateRunning {
		t.Fatalf("task closed although its liveness could not be read: %s", got.State)
	}
	if logged := output.String(); !strings.Contains(logged, "WARN") || !strings.Contains(logged, quiet.ID) {
		t.Fatalf("skipped task left no warning:\n%s", logged)
	}

	// The same task is closed once its liveness is known.
	closeIdleTasks(t.Context(), tasks, attempt.New(openAttempts(t)), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateDone {
		t.Fatalf("quiet task with no live attempt = %s, want done", got.State)
	}
}

func openAttempts(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return book
}

// testLedger opens a ledger that lives as long as the test.
func testLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

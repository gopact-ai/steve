package app

import (
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
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

// Nothing in flight is not the same as nothing left: a quiet task whose
// console turn still waits on the owner's answer is not closed as done, and
// is closed by a later pass once that turn has settled.
func TestIdleSweepKeepsATaskWhoseContinuationIsUnsettled(t *testing.T) {
	book := testLedger(t)
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	const conversation = "console:quiet"
	quiet, err := tasks.Create(task.Task{Transport: "console", Goal: "quiet chat", Channel: conversation, Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(quiet.ID, "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	store := func(exchange consoleapi.ExchangeState, question string) {
		t.Helper()
		state := console.DurableState{
			Exchanges: map[string][]console.DurableExchange{conversation: {{Exchange: consoleapi.Exchange{ID: "e1", Conversation: conversation, Input: "quiet chat", State: exchange}}}},
			Questions: map[string]consoleapi.PendingQuestion{"q1": {ID: "q1", Conversation: conversation, ExchangeID: "e1", TaskID: quiet.ID, State: question}},
		}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, state) }); err != nil {
			t.Fatal(err)
		}
	}
	store(consoleapi.ExchangeAwaitingUser, "pending")
	time.Sleep(5 * time.Millisecond)

	closeIdleTasks(t.Context(), tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateRunning {
		t.Fatalf("quiet task closed while its turn waits on the owner: %s, want running", got.State)
	}

	store(consoleapi.ExchangeDone, "answered")
	closeIdleTasks(t.Context(), tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateDone {
		t.Fatalf("quiet task after its turn settled = %s, want done", got.State)
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

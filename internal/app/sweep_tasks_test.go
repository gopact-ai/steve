package app

import (
	"fmt"
	"runtime"
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
	book := testLedger(t)
	tasks, err := task.OpenLedger(book)
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
	closeIdleTasks(t.Context(), book, tasks, attempt.New(unreadable), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateRunning {
		t.Fatalf("task closed although its liveness could not be read: %s", got.State)
	}
	if logged := output.String(); !strings.Contains(logged, "WARN") || !strings.Contains(logged, quiet.ID) {
		t.Fatalf("skipped task left no warning:\n%s", logged)
	}

	// The same task is closed once its liveness is known.
	closeIdleTasks(t.Context(), book, tasks, attempt.New(openAttempts(t)), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateDone {
		t.Fatalf("quiet task with no live attempt = %s, want done", got.State)
	}
}

// quietConsoleTask opens a running console task in conversation that the
// sweep finds quiet.
func quietConsoleTask(t *testing.T, conversation string) (*ledger.Ledger, *task.Store, task.Task) {
	t.Helper()
	book := testLedger(t)
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := tasks.Create(task.Task{Transport: "console", Goal: "quiet chat", Channel: conversation, Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(quiet.ID, "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	return book, tasks, quiet
}

// Nothing in flight is not the same as nothing left: a quiet task whose
// console turn still waits on the owner's answer is not closed as done, and
// is closed by a later pass once that turn has settled.
func TestIdleSweepKeepsATaskWhoseContinuationIsUnsettled(t *testing.T) {
	const conversation = "console:quiet"
	book, tasks, quiet := quietConsoleTask(t, conversation)
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

	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateRunning {
		t.Fatalf("quiet task closed while its turn waits on the owner: %s, want running", got.State)
	}

	store(consoleapi.ExchangeDone, "answered")
	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateDone {
		t.Fatalf("quiet task after its turn settled = %s, want done", got.State)
	}
}

// A reset goes ahead of a line queued behind it, and the line runs under
// the task that comes next. Nothing comes next after an idle close: a line
// still queued in the conversation is about to continue the quiet task, so
// the task stays open until the line has run.
func TestIdleSweepKeepsATaskALineIsQueuedFor(t *testing.T) {
	const conversation = "console:quiet"
	book, tasks, quiet := quietConsoleTask(t, conversation)
	store := func(exchange consoleapi.ExchangeState) {
		t.Helper()
		state := console.DurableState{
			Exchanges: map[string][]console.DurableExchange{conversation: {{Exchange: consoleapi.Exchange{ID: "e1", Conversation: conversation, Input: "one more thing", State: exchange}}}},
		}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, state) }); err != nil {
			t.Fatal(err)
		}
	}
	store(consoleapi.ExchangeQueued)
	time.Sleep(5 * time.Millisecond)

	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateRunning {
		t.Fatalf("quiet task closed with a line queued for it: %s, want running", got.State)
	}

	store(consoleapi.ExchangeDone)
	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond)
	if got, _ := tasks.Get(quiet.ID); got.State != task.StateDone {
		t.Fatalf("quiet task after the queued line ran = %s, want done", got.State)
	}
}

// What a pass costs for each quiet task is what that task holds, not the
// history the ledger holds for other tasks and other conversations: the
// check for one task does not read every attempt and every console line
// there is, and a pass does not repeat for each task what it can read once.
func TestIdleSweepPerTaskCostDoesNotGrowWithHistory(t *testing.T) {
	book := testLedger(t)
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	quiet := 0
	// pass sweeps n newly quiet tasks and returns what it allocated.
	pass := func(n int) int64 {
		t.Helper()
		ids := make([]string, n)
		for i := range ids {
			quiet++
			tracked, err := tasks.Create(task.Task{Transport: "console", Goal: "quiet chat", Channel: fmt.Sprintf("console:quiet-%d", quiet), Member: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(tracked.ID, "worker", "", ""); err != nil {
				t.Fatal(err)
			}
			ids[i] = tracked.ID
		}
		time.Sleep(5 * time.Millisecond)
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		closeIdleTasks(t.Context(), book, tasks, attempts, nil, time.Millisecond)
		runtime.ReadMemStats(&after)
		for _, id := range ids {
			if got, _ := tasks.Get(id); got.State != task.StateDone {
				t.Fatalf("quiet task %s after the pass = %s, want done", id, got.State)
			}
		}
		return int64(after.Mallocs - before.Mallocs)
	}
	perTask := func() int64 {
		t.Helper()
		one := pass(1)
		return (pass(6) - one) / 5
	}
	fresh := perTask()

	if _, err := book.DB().Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<5000)
		INSERT INTO operations SELECT 'history-'||n,'attempt','bound',1,1,
		json_object('id','history-'||n,'task_id','other-'||n,'turn_id','other-'||n,'started_at','2026-09-01T00:00:00Z','session_settled',json('true')),
		'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z' FROM seq`); err != nil {
		t.Fatal(err)
	}
	history := console.DurableState{Exchanges: map[string][]console.DurableExchange{}}
	for i := range 1000 {
		conversation := fmt.Sprintf("console:history-%d", i)
		history.Exchanges[conversation] = []console.DurableExchange{{Exchange: consoleapi.Exchange{ID: fmt.Sprintf("history-%d", i), Conversation: conversation, Input: "long done", State: consoleapi.ExchangeDone}}}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, history) }); err != nil {
		t.Fatal(err)
	}
	aged := perTask()
	t.Logf("allocations per quiet task: %d fresh, %d with history", fresh, aged)
	if aged > fresh+2000 {
		t.Fatalf("a pass costs more for each quiet task as history grows: %d allocations per task, %d without the history", aged, fresh)
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

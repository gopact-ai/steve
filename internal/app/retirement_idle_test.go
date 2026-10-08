package app

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func TestIdleSweepSharesConversationRetirementAdmission(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	created, err := tasks.Create(task.Task{Channel: "console:idle", Member: "agent", Goal: "quiet", State: task.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	created, err = tasks.Advance(created.ID, task.StateRunning)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turntest.New(t, func(o *turntest.Options) { o.Ledger = book; o.Tasks = tasks })
	idleClose := turn.IdleCloseReservation(coordinator)
	time.Sleep(3 * time.Millisecond)
	release, err := idleClose(t.Context(), created.Channel)
	if err != nil {
		t.Fatal(err)
	}
	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond, idleClose)
	got, _ := tasks.Get(created.ID)
	if got.State != task.StateRunning || got.ExecutionEpoch != created.ExecutionEpoch {
		t.Fatal("idle sweep closed a task whose context is retiring")
	}
	release()
	closeIdleTasks(t.Context(), book, tasks, attempt.New(book), nil, time.Millisecond, idleClose)
	got, _ = tasks.Get(created.ID)
	if got.State != task.StateDone {
		t.Fatalf("idle closure stayed fenced after release: %s", got.State)
	}
}

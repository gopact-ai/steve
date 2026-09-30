package console

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

func TestResumeWaitsUntilTheOriginalStopIsConfirmed(t *testing.T) {
	s, h, driver := openStopWaitConsole(t, task.StatePaused, true)
	awaitStopWait(t, s)
	call := nextCall(t, h)
	tracked, _ := driver.tasks.Get(driver.id)
	admission := task.ResumeAdmission{ID: "resume-check", TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	revived := false
	err := s.Resume(t.Context(), "main", tracked.ID, "worker", "continue", "continue", admission, func(string, string) error { revived = true; return nil })
	if !errors.Is(err, turn.ErrResumeAwaitsStop) {
		t.Fatalf("resume while stop pending = %v, want stop refusal", err)
	}
	if revived {
		t.Fatal("a refused resume revived the session")
	}
	if current, _ := driver.tasks.Get(tracked.ID); current.State != task.StatePaused {
		t.Fatalf("refused resume left task %s", current.State)
	}
	driver.confirmed.Store(true)
	answerRecheck(t, s, awaitStopWait(t, s))
	awaitExchange(t, s, "e1")
	if err := s.Resume(t.Context(), "main", tracked.ID, "worker", "continue", "continue", admission, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	call.finish <- nil
}

func TestSettlingQueueDoesNotReadTheLedgerUnderItsLock(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(&echo{}, "owner", nil)
	s.book = book
	e := &queuedExchange{Exchange: Exchange{State: consoleapi.ExchangeAwaitingUser}, RecoveryStopPending: stopRequested, RecoveryStopTask: "task", stopSetAside: true}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = book.Read(t.Context(), func(*ledger.ReadTx) error { entered <- struct{}{}; <-release; return nil })
		}()
	}
	for range 4 {
		<-entered
	}
	checked := make(chan bool, 1)
	go func() { s.mu.Lock(); defer s.mu.Unlock(); checked <- s.stopSettlingLocked(e) }()
	select {
	case settling := <-checked:
		if !settling {
			t.Error("known stop wait still holds the queue")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("queue waited for a ledger connection while holding its lock")
	}
	close(release)
	for range 4 {
		<-done
	}
}

func TestRejectedResumeHasAVisibleReply(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	e := &queuedExchange{Exchange: Exchange{ID: "resume", Conversation: "console:main", ExpectedTask: "task", State: consoleapi.ExchangeQueued}, done: make(chan struct{})}
	s.exchanges[e.Conversation] = []*queuedExchange{e}
	s.mu.Lock()
	err := s.rejectResumeLocked(e, task.ErrExecutionStopped)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	replies := s.Replies("main")
	if len(replies) != 1 || replies[0].ExchangeID != e.ID || !strings.Contains(replies[0].Text, "task") {
		t.Fatalf("rejected resume has no explanation in the conversation: %+v", replies)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

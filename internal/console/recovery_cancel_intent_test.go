package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
)

func TestRestartFinishesTheDurableCancelIntent(t *testing.T) {
	for _, state := range []task.State{task.StateRunning, task.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			doc := recoveryDocument()
			var saved transcript
			raw, _, _ := doc.Load()
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			e := saved.Exchanges["console:main"][0]
			e.State = consoleapi.ExchangeAwaitingUser
			e.RecoveryStopPending, e.RecoveryStopTask, e.RecoveryCancelPending = stopRequested, "task-1", true
			raw, _ = json.Marshal(saved)
			if err := doc.Save(raw); err != nil {
				t.Fatal(err)
			}
			s := impatient(New(&echo{}, "owner", nil))
			s.EnableRetainedRecovery(t.Context())
			s.recoveryStopEvery = 10 * time.Millisecond
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := newCancelledTaskDriver()
			driver.setAside(state, "task-1")
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			awaitStops(t, &driver.stops)
			deadline := time.Now().Add(time.Second)
			for !driver.explicit.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !driver.explicit.Load() {
				t.Fatal("restart lost the owner's durable cancel intent")
			}
			if driver.state("task-1") != task.StateCancelled {
				t.Fatal("restart left task resumable")
			}
		})
	}
}

func TestCancelIntentDoesNotPromiseTheTaskWillStayPaused(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	driver := newCancelledTaskDriver()
	driver.setAside(task.StatePaused, "task-1")
	s.recoveryDriver = driver
	e := &queuedExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main"}, RecoveryCancelPending: true}
	w := &recoveryStopWait{s: s, e: e, ctx: t.Context()}
	if w.paused() {
		t.Fatal("pending explicit cancellation promised the task would stay paused")
	}
}

type refuseCancelIntentDoc struct {
	*memDoc
	reject atomic.Bool
}

func (d *refuseCancelIntentDoc) Save(raw []byte) error {
	if d.reject.Load() && bytes.Contains(raw, []byte(`"recovery_cancel_pending":true`)) {
		return errors.New("cancel intent write refused")
	}
	return d.memDoc.Save(raw)
}

func TestRefusedCancelIntentNeverReachesTheTask(t *testing.T) {
	doc := &refuseCancelIntentDoc{memDoc: recoveryDocument()}
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := newCancelledTaskDriver()
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	awaitOffer(t, s, "retry")
	doc.reject.Store(true)
	_, err := s.SendCommand(t.Context(), "main", "/cancel", "reject-intent")
	if err == nil || !strings.Contains(err.Error(), "cancel intent write refused") {
		t.Fatalf("refused cancel intent = %v", err)
	}
	if driver.stops.Load() != 0 {
		t.Fatal("stop executed without its intent being durable")
	}
	s.mu.Lock()
	e := s.exchanges["console:main"][0]
	bad := e.RecoveryCancelPending || e.RecoveryStopPending != "" || e.RecoveryStopTask != ""
	s.mu.Unlock()
	if bad {
		t.Fatal("refused intent was not rolled back")
	}
}

func TestTaskStateRefreshMovesTheQueueWhileStopIsBlocked(t *testing.T) {
	s, h, driver := openStopWaitConsole(t, task.StateRunning, true)
	awaitStopWait(t, s)
	driver.hold.Store(true)
	defer close(driver.held)
	card := awaitStopWait(t, s)
	answerRecheck(t, s, card)
	awaitStops(t, &driver.stops)
	if _, err := driver.tasks.SetAside(driver.id, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	s.TasksSetAside()
	call := nextCall(t, h)
	defer release(call)
	if call.req.Input != "follow-up" {
		t.Fatalf("wrong queued input: %q", call.req.Input)
	}
}

func TestTaskStateRefreshDoesNotStartUndurableInput(t *testing.T) {
	s, h, driver := openStopWaitConsole(t, task.StateRunning, true)
	awaitStopWait(t, s)
	driver.hold.Store(true)
	defer close(driver.held)
	answerRecheck(t, s, awaitStopWait(t, s))
	awaitStops(t, &driver.stops)
	if _, err := driver.tasks.SetAside(driver.id, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := s.book.DB().Exec(`CREATE TRIGGER reject_queue_start BEFORE UPDATE ON bindings WHEN new.kind='console-store' BEGIN SELECT RAISE(ABORT, 'queue write refused'); END`); err != nil {
		t.Fatal(err)
	}
	s.TasksSetAside()
	noCall(t, h)
	if got := queuedByID(s, "e2"); got.State != consoleapi.ExchangeQueued {
		t.Fatalf("undurable input started: %+v", got)
	}
	if _, err := s.book.DB().Exec(`DROP TRIGGER reject_queue_start`); err != nil {
		t.Fatal(err)
	}
	s.TasksSetAside()
	call := nextCall(t, h)
	defer release(call)
	if call.req.Input != "follow-up" {
		t.Fatal("wrong input")
	}
}

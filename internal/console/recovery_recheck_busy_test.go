package console

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// The first pass already observed the stop as unconfirmed. Holding its return
// makes a subsequent user recheck arrive while that pass still owns busy.
type heldRecheckResult struct {
	*cancelledTaskDriver
	sampled chan struct{}
	release chan struct{}
}

func (d *heldRecheckResult) StopRetainedTask(ctx context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	result, err := d.cancelledTaskDriver.StopRetainedTask(ctx, id, req, cancel)
	if d.stops.Load() == 1 {
		close(d.sampled)
		select {
		case <-d.release:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	return result, err
}

func TestRecoveryRecheckAcceptedDuringAStopRunsAfterThatPass(t *testing.T) {
	for _, state := range []task.State{task.StateCancelled, task.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			s := impatient(New(&echo{}, "owner", nil))
			s.recoveryStopEvery = time.Hour
			s.EnableRetainedRecovery(ctx)
			if err := s.Persist(recoveryDocument()); err != nil {
				t.Fatal(err)
			}
			driver := &heldRecheckResult{cancelledTaskDriver: newCancelledTaskDriver(), sampled: make(chan struct{}), release: make(chan struct{})}
			driver.setAside(state, "task-1")
			s.recoveryDriver = driver
			e := s.exchanges["console:main"][0]
			exchangeCtx, stopExchange := context.WithCancel(ctx)
			e.State, e.ctx, e.cancel = consoleapi.ExchangeAwaitingUser, exchangeCtx, stopExchange
			e.RecoveryStopPending, e.RecoveryStopTask = stopRequested, "task-1"
			w := &recoveryStopWait{s: s, e: e, ctx: exchangeCtx, requester: "owner", reason: stopRequested, changed: make(chan struct{}, 1), base: consoleapi.PendingQuestion{Conversation: e.Conversation, ExchangeID: e.ID}}
			var released sync.Once
			runDone := make(chan struct{})
			t.Cleanup(func() {
				released.Do(func() { close(driver.release) })
				cancel()
				select {
				case <-runDone:
				case <-time.After(3 * time.Second):
					t.Error("recheck waiter did not exit")
				}
				s.workers.Wait()
			})
			w.check()
			go func() { defer close(runDone); w.run() }()
			card := awaitStopWait(t, s)
			select {
			case <-driver.sampled:
			case <-time.After(2 * time.Second):
				t.Fatal("first stop did not sample the unconfirmed result")
			}
			driver.confirmed.Store(true)
			answerRecheck(t, s, card)
			// A new pending question means run consumed the accepted answer;
			// the old stop is still held inside the driver at this boundary.
			consumed := false
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				for _, question := range s.Questions("main") {
					if question.ID != card.ID && question.State == "pending" {
						consumed = true
					}
				}
				if consumed {
					break
				}
			}
			if !consumed {
				t.Fatal("accepted recheck was not consumed")
			}
			w.mu.Lock()
			busy := w.busy
			w.mu.Unlock()
			if !busy || driver.stops.Load() != 1 {
				t.Fatal("the controlled first pass was not still exclusive")
			}
			released.Do(func() { close(driver.release) })
			if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
				t.Fatalf("recheck did not settle the original exchange: %+v", got)
			}
			if driver.stops.Load() != 2 {
				t.Fatalf("recheck calls=%d, want the original and one accepted follow-up", driver.stops.Load())
			}
			if driver.state("task-1") != state {
				t.Fatal("automatic recheck changed the task's set-aside state")
			}
			awaitExchange(t, s, "e2")
		})
	}
}

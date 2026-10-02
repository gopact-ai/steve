package console

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

type sequentialStopDriver struct {
	*cancelledTaskDriver
	entered chan int
	release chan struct{}
	calls   atomic.Int32
	active  atomic.Int32
	maximum atomic.Int32
	mu      sync.Mutex
	intents []bool
}

func (d *sequentialStopDriver) StopRetainedTask(ctx context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	active := d.active.Add(1)
	defer d.active.Add(-1)
	for previous := d.maximum.Load(); active > previous; previous = d.maximum.Load() {
		if d.maximum.CompareAndSwap(previous, active) {
			break
		}
	}
	call := int(d.calls.Add(1))
	d.mu.Lock()
	d.intents = append(d.intents, cancel)
	d.mu.Unlock()
	result, err := d.cancelledTaskDriver.StopRetainedTask(ctx, id, req, cancel)
	d.entered <- call
	select {
	case <-d.release:
	case <-ctx.Done():
		return turn.Result{}, ctx.Err()
	}
	return result, err
}
func newRecheckWaitFixture(t *testing.T) (*recoveryStopWait, *sequentialStopDriver, context.CancelFunc) {
	t.Helper()
	life, end := context.WithCancel(t.Context())
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(life)
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	d := &sequentialStopDriver{cancelledTaskDriver: newCancelledTaskDriver(), entered: make(chan int, 8), release: make(chan struct{})}
	d.setAside(task.StatePaused, "task-1")
	s.recoveryDriver = d
	ctx, cancel := context.WithCancel(life)
	e := s.exchanges["console:main"][0]
	e.State, e.ctx = consoleapi.ExchangeAwaitingUser, ctx
	e.RecoveryStopPending, e.RecoveryStopTask = stopRequested, "task-1"
	w := &recoveryStopWait{s: s, e: e, ctx: ctx, requester: "owner", reason: stopRequested, changed: make(chan struct{}, 1)}
	t.Cleanup(func() { cancel(); end(); close(d.release); s.workers.Wait() })
	return w, d, cancel
}
func waitForRecheckIdle(t *testing.T, w *recoveryStopWait) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		w.mu.Lock()
		busy := w.busy
		w.mu.Unlock()
		if !busy {
			return
		}
	}
	t.Fatal("stop worker did not release its busy state")
}
func expectStopPass(t *testing.T, d *sequentialStopDriver, want int) {
	t.Helper()
	select {
	case got := <-d.entered:
		if got != want {
			t.Fatalf("stop pass=%d want=%d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted recheck did not enter its next stop pass")
	}
}
func releaseStopPass(t *testing.T, d *sequentialStopDriver) {
	t.Helper()
	select {
	case d.release <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("stop pass did not consume its release")
	}
}

func TestExplicitRechecksCoalesceWithoutLosingANewRequestDuringTheNextPass(t *testing.T) {
	w, d, _ := newRecheckWaitFixture(t)
	w.checkIntent(true)
	expectStopPass(t, d, 1)
	for range 5 {
		w.recheckNow()
		w.check()
	}
	if d.calls.Load() != 1 {
		t.Fatal("rechecks started concurrent stops")
	}
	releaseStopPass(t, d)
	expectStopPass(t, d, 2)
	for range 5 {
		w.recheckNow()
		w.check()
	}
	if d.calls.Load() != 2 {
		t.Fatal("a queued recheck overlapped the active follow-up")
	}
	releaseStopPass(t, d)
	expectStopPass(t, d, 3)
	releaseStopPass(t, d)
	waitForRecheckIdle(t, w)
	if d.calls.Load() != 3 || d.maximum.Load() != 1 {
		t.Fatalf("calls=%d concurrent=%d", d.calls.Load(), d.maximum.Load())
	}
	w.mu.Lock()
	checks, pending := w.checks, w.recheckPending
	w.mu.Unlock()
	if checks != 3 || pending {
		t.Fatalf("checks=%d pending=%v", checks, pending)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.intents) != 3 || !d.intents[0] || d.intents[1] || d.intents[2] {
		t.Fatalf("stop intentions=%v", d.intents)
	}
}

func TestPeriodicRechecksDoNotQueueAStopWhileOneIsRunning(t *testing.T) {
	w, d, _ := newRecheckWaitFixture(t)
	w.check()
	expectStopPass(t, d, 1)
	for range 5 {
		w.check()
	}
	releaseStopPass(t, d)
	waitForRecheckIdle(t, w)
	w.mu.Lock()
	checks, pending := w.checks, w.recheckPending
	w.mu.Unlock()
	if d.calls.Load() != 1 || checks != 1 || pending {
		t.Fatalf("periodic checks accumulated: calls=%d checks=%d pending=%v", d.calls.Load(), checks, pending)
	}
	if d.state("task-1") != task.StatePaused {
		t.Fatal("periodic observation cancelled the paused task")
	}
}

func TestPendingExplicitRecheckEndsWithItsWaitOrAConfirmedStop(t *testing.T) {
	for _, why := range []string{"context ends", "first pass confirms"} {
		t.Run(why, func(t *testing.T) {
			w, d, cancel := newRecheckWaitFixture(t)
			if why == "first pass confirms" {
				d.confirmed.Store(true)
			}
			w.check()
			expectStopPass(t, d, 1)
			w.recheckNow()
			if why == "context ends" {
				cancel()
			}
			// The current native stop belongs to the service lifetime, not the
			// question waiter; let that in-flight pass return before checking it.
			releaseStopPass(t, d)
			waitForRecheckIdle(t, w)
			w.mu.Lock()
			checks, pending := w.checks, w.recheckPending
			w.mu.Unlock()
			if d.calls.Load() != 1 || checks != 1 || pending {
				t.Fatalf("finished wait repeated stop: calls=%d checks=%d pending=%v", d.calls.Load(), checks, pending)
			}
			if why == "first pass confirms" {
				if got := awaitExchange(t, w.s, "e1"); got.State != consoleapi.ExchangeCancelled {
					t.Fatalf("confirmed pass left %s", got.State)
				}
				awaitExchange(t, w.s, "e2")
			}
		})
	}
}

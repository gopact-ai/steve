package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestStopFindsAllTaskAttemptsAndWaitsForOwners(t *testing.T) {
	tasks, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := tasks.Create(task.Task{Channel: "c", Member: "parent"})
	child, _ := tasks.Spawn(parent.ID, task.Task{Member: "child"})
	other, _ := tasks.Create(task.Task{Channel: "c", Member: "child"})
	r := New(t.Context(), tasks)
	a, _ := r.Begin(t.Context(), Key{TaskID: parent.ID, InstanceID: "plan"})
	b, _ := r.Begin(r.Detached(a.Context()), Key{TaskID: child.ID, AttemptID: "one"})
	c, _ := r.Begin(b.Context(), Key{TaskID: child.ID, AttemptID: "two"})
	keep, _ := r.Begin(t.Context(), Key{TaskID: other.ID, AttemptID: "other"})
	defer keep.Finish(nil)
	a.Finish(nil)
	if b.Context().Err() != nil {
		t.Fatal("normal parent completion cancelled child")
	}
	ids, err := tasks.SetAside(parent.ID, task.StateCancelled)
	if err != nil {
		t.Fatal(err)
	}
	wait := r.Stop(ids, task.ErrExecutionStopped)
	if b.Context().Err() == nil || c.Context().Err() == nil || keep.Context().Err() != nil {
		t.Fatal("stop missed attempts or crossed task boundary")
	}
	if _, err := r.Begin(t.Context(), Key{TaskID: child.ID, AttemptID: "late"}); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("late start: %v", err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := wait.Wait(bounded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stop reported completion before owner cleanup", err)
	}
	b.Finish(nil)
	c.Finish(nil)
	if err := wait.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownClosesAdmissionAndWaitsForCleanup(t *testing.T) {
	r := New(t.Context(), nil)
	scope, err := r.Begin(t.Context(), Key{InstanceID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- r.Shutdown(t.Context()) }()
	<-scope.Context().Done()
	if _, err := r.Begin(t.Context(), Key{InstanceID: "late"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted during shutdown: %v", err)
	}
	select {
	case err := <-finished:
		t.Fatalf("shutdown completed before owner cleanup: %v", err)
	default:
	}
	unresolved := errors.New("writer stop not confirmed")
	scope.Finish(unresolved)
	if err := <-finished; !errors.Is(err, unresolved) {
		t.Fatalf("lost unresolved writer: %v", err)
	}
	if _, err := r.Begin(t.Context(), Key{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted after shutdown: %v", err)
	}
}

// Detached work waiting on a person must not hold the silence clock of
// the turn it came from: that turn keeps its own hang detection.
func TestDetachedWorkDoesNotHoldItsOriginsSilenceClock(t *testing.T) {
	origin, stop, _ := idle.WithTimeout(t.Context(), 30*time.Millisecond)
	defer stop()
	release := idle.Hold(New(t.Context(), nil).Detached(origin))
	defer release()
	select {
	case <-origin.Done():
	case <-time.After(time.Second):
		t.Fatal("detached work held its origin's silence clock")
	}
}

// Detached work does not end on its origin's silence: when the service
// lifetime ends, work derived from a Detached context does not read as a
// silence clock that ran out, even though the turn it came from did.
func TestDetachedWorkDoesNotEndOnItsOriginsSilence(t *testing.T) {
	origin, stop, _ := idle.WithTimeout(t.Context(), time.Millisecond)
	defer stop()
	turn, cancel := context.WithCancel(origin)
	defer cancel()
	<-turn.Done()
	lifetime, end := context.WithCancel(t.Context())
	detached := New(lifetime, nil).Detached(turn)
	work, finish := context.WithCancel(detached)
	defer finish()
	end()
	<-work.Done()
	if !idle.Expired(turn) || idle.Expired(detached) || idle.Expired(work) {
		t.Fatalf("expired: turn=%v detached=%v work=%v", idle.Expired(turn), idle.Expired(detached), idle.Expired(work))
	}
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

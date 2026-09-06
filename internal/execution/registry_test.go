package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func TestStopFindsAllTaskAttemptsAndWaitsForOwners(t *testing.T) {
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
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

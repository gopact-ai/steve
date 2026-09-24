package execution

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionClosesRegistryAdmissionBeforeReleasingIdleGate(t *testing.T) {
	tasks, err := task.OpenLedger(taskBook(t))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := tasks.Create(task.Task{Channel: "chat"})
	if _, err := tasks.Advance(root.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	registry := New(t.Context(), tasks)
	entered, release := make(chan struct{}), make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		completed <- registry.WhileTaskIdle(root.ID, func() error {
			close(entered)
			<-release
			_, err := tasks.CompleteRoot(t.Context(), root.ID, root.Channel, func(*ledger.Tx, map[string]bool) error { return nil })
			return err
		})
	}()
	<-entered
	admitted := make(chan error, 1)
	go func() {
		scope, err := registry.Begin(t.Context(), Key{TaskID: root.ID})
		if err == nil {
			scope.Finish(nil)
		}
		admitted <- err
	}()
	close(release)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("late admission: %v", err)
	}
}

func TestCompletionWaitsForDescendantScopeCleanup(t *testing.T) {
	tasks, err := task.OpenLedger(taskBook(t))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := tasks.Create(task.Task{Channel: "chat"})
	child, _ := tasks.Spawn(root.ID, task.Task{})
	registry := New(t.Context(), tasks)
	scope, err := registry.Begin(t.Context(), Key{TaskID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = registry.WhileTaskIdle(root.ID, func() error { called = true; return nil })
	if !errors.Is(err, task.ErrCompleteBusy) || called {
		t.Fatalf("child scope ignored: %v", err)
	}
	scope.Finish(nil)
	if err := registry.WhileTaskIdle(root.ID, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("idle gate: %v", err)
	}
}

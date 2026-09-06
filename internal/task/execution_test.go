package task

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestSetAsideRevokesWholeTreeAndSurvivesResume(t *testing.T) {
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s, err := OpenLedger(l, "")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Create(Task{Channel: "chat", Member: "parent"})
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.Create(Task{Channel: "chat", Member: "child"})
	old, _ := s.ExecutionToken(child.ID)
	keep, _ := s.ExecutionToken(other.ID)
	ids, err := s.SetAside(root.ID, StatePaused)
	if err != nil || len(ids) != 2 {
		t.Fatalf("stop %v: %v", ids, err)
	}
	if _, err := s.Spawn(child.ID, Task{Member: "late"}); err == nil {
		t.Fatal("paused tree spawned a child")
	}
	if err := l.Update(context.Background(), func(tx *ledger.Tx) error { return CheckExecutionTx(tx, &old) }); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("old token: %v", err)
	}
	if err := s.CheckExecution(keep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(root.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(child.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckExecution(old); !errors.Is(err, ErrExecutionStopped) {
		t.Fatal("resume revived stale permission")
	}
	reloaded, err := OpenLedger(l, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.CheckExecution(old); !errors.Is(err, ErrExecutionStopped) {
		t.Fatal("restart revived permission")
	}
}

func TestNormalCompletionDoesNotRevokeChildOrPendingResult(t *testing.T) {
	s, _ := newStore(t)
	root := mustCreate(t, s, "parent", "chat")
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := s.ExecutionToken(child.ID)
	if _, err := s.Advance(root.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckExecution(token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(child.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckExecution(token); err != nil {
		t.Fatal("normal completion revoked pending result", err)
	}
	if _, err := s.SetAside(root.ID, StateCancelled); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckExecution(token); !errors.Is(err, ErrExecutionStopped) {
		t.Fatal("explicit stop kept pending result authorized")
	}
}

package attempt

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestStoppedExecutionCannotCompleteAfterResume(t *testing.T) {
	s, _ := newService(t)
	tasks, err := task.OpenLedger(s.l, "")
	if err != nil {
		t.Fatal(err)
	}
	work, _ := tasks.Create(task.Task{Channel: "chat"})
	token, _ := tasks.ExecutionToken(work.ID)
	r, err := s.Open(t.Context(), Spec{ID: "old", TaskID: work.ID, Execution: &token, Kind: KindStep, Project: "p", Workspace: worktree("wt-old", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{Prepared, Running, BindReady} {
		if _, err := s.Advance(t.Context(), r.ID, state, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tasks.SetAside(work.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(work.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(t.Context(), r.ID, "late", Completion{Result: Result{Artifact: "late-result"}, Binding: &NameBinding{Name: "result"}}); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("completion=%v", err)
	}
	if _, found, err := s.l.Name(t.Context(), "result"); err != nil || found {
		t.Fatalf("stopped execution changed name: %v %v", found, err)
	}
	if _, err := s.FailWith(t.Context(), r.ID, "cleanup", "cancelled", &Usage{Input: 3, Reported: true}); err != nil {
		t.Fatal(err)
	}
	closed, _ := s.Get(t.Context(), r.ID)
	if closed.Usage == nil || closed.Usage.Input != 3 {
		t.Fatal("cancelled spend missing")
	}
}

func TestUnconfirmedWriterBlocksReplacementBeyondLeaseExpiry(t *testing.T) {
	s, clock := newService(t)
	ws := worktree("wt-old", "p")
	r, err := s.Open(t.Context(), Spec{ID: "old", Project: "p", Workspace: ws, Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsettled(t.Context(), r.ID, "test", errors.New("cancel not confirmed"), nil); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Hour)
	if got, err := s.Sweep(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("sweeper removed quarantine: %+v %v", got, err)
	}
	if got, err := s.ExpireAll(t.Context(), "restart"); err != nil || len(got) != 0 {
		t.Fatalf("restart removed quarantine: %+v %v", got, err)
	}
	if live, err := s.Live(t.Context()); err != nil || len(live) != 1 || !live[0].Unsettled {
		t.Fatalf("writer vanished from live resources: %+v %v", live, err)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "new", Project: "p", Workspace: ws, Scope: ScopePathSet}); err == nil {
		t.Fatal("lease expiry admitted replacement over unconfirmed writer")
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckWriterTx(tx, ws.Node, ws.Path) }); err == nil {
		t.Fatal("landing admission ignored unconfirmed writer")
	}
	if _, err := s.ConfirmStopped(t.Context(), r.ID, "operator", ""); err == nil {
		t.Fatal("empty evidence cleared quarantine")
	}
	if _, err := s.ConfirmStopped(t.Context(), r.ID, "operator", "original worker physically exited"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "confirmed-new", Project: "p", Workspace: ws, Scope: ScopePathSet}); err != nil {
		t.Fatal("confirmed stop did not free writer", err)
	}
}

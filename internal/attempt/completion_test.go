package attempt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func readyAttempt(t *testing.T, s *Service, id string) Record {
	t.Helper()
	r, err := s.Open(t.Context(), Spec{ID: id, Kind: KindStep, Project: "p", Workspace: worktree("wt-"+id, "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{Prepared, Running, BindReady} {
		r, err = s.Advance(t.Context(), id, state, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSessionSettled(t.Context(), r.ID, "test worker finished"); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCompletionCommitsNameOutcomeAndSpendTogether(t *testing.T) {
	s, _ := newService(t)
	r := readyAttempt(t, s, "work")
	spend := &Usage{Model: "model", Input: 100, Output: 20, Context: 500, Reported: true}
	completed, err := s.Complete(t.Context(), r.ID, "test", Completion{Result: Result{Artifact: "result", Summary: "done"}, Usage: spend, Binding: &NameBinding{Name: "result/work"}})
	if err != nil {
		t.Fatal(err)
	}
	name, ok, err := s.l.Name(t.Context(), "result/work")
	if err != nil || !ok || name.Artifact != "result" || name.Version != 1 || completed.State != Bound || completed.EndedAt.IsZero() || completed.Result.Summary != "done" || *completed.Usage != *spend {
		t.Fatalf("completion=%+v name=%+v found=%v err=%v", completed, name, ok, err)
	}
	for _, lease := range r.Leases {
		if err := s.l.Check(t.Context(), lease); !errors.Is(err, ledger.ErrStale) {
			t.Fatalf("completed lease still live: %v", err)
		}
	}
	events, err := s.History(t.Context(), r.ID)
	if err != nil || events[len(events)-1].To != string(Bound) || len(events[len(events)-1].Fencings) != len(r.Leases) {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestCompletionCASFailureLeavesAttemptAndLeasesUntouched(t *testing.T) {
	s, _ := newService(t)
	r := readyAttempt(t, s, "work")
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.CompareAndSetName("result/work", 0, "winner"); return err }); err != nil {
		t.Fatal(err)
	}
	_, err := s.Complete(t.Context(), r.ID, "test", Completion{Result: Result{Artifact: "loser"}, Usage: &Usage{Input: 100, Reported: true}, Binding: &NameBinding{Name: "result/work"}})
	if !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("complete conflict=%v", err)
	}
	got, _ := s.Get(t.Context(), r.ID)
	name, _, _ := s.l.Name(t.Context(), "result/work")
	if got.State != BindReady || got.Revision != r.Revision || got.Result != nil || got.Usage != nil || !got.EndedAt.IsZero() || name.Artifact != "winner" {
		t.Fatalf("partial completion: attempt=%+v name=%+v", got, name)
	}
	for _, lease := range r.Leases {
		if err := s.l.Check(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompletionCannotBindAfterLeaseLossOrTakeover(t *testing.T) {
	for _, lost := range []string{"expired", "superseded"} {
		t.Run(lost, func(t *testing.T) {
			s, clock := newService(t)
			r := readyAttempt(t, s, "old")
			if lost == "expired" {
				clock.t = clock.t.Add(2 * time.Minute)
			} else {
				if _, err := s.Supersede(t.Context(), r.ID, Spec{ID: "new", Kind: KindStep, Project: "p", Workspace: worktree("wt-new", "p"), Scope: ScopePathSet}, "test"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.Complete(t.Context(), r.ID, "zombie", Completion{Result: Result{Artifact: "zombie"}, Binding: &NameBinding{Name: "result/work"}})
			if err == nil {
				t.Fatal("lost attempt completed")
			}
			if _, ok, err := s.l.Name(t.Context(), "result/work"); err != nil || ok {
				t.Fatalf("lost attempt moved name: found=%v err=%v", ok, err)
			}
		})
	}
}

func TestSupersedeKeepsForeignResourcesNewHolder(t *testing.T) {
	s, clock := newService(t)
	foreign, err := ledger.Open(t.TempDir(), ledger.Options{Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { foreign.Close() })
	foreign.SetRegion("remote")
	s.l.RegisterIssuer("remote", foreign)
	old, err := s.Open(t.Context(), Spec{ID: "old", Kind: KindStep, Project: "p", Node: "node-a", Harness: "mock", Slots: 1, Region: "remote", Workspace: worktree("wt-old", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(2 * time.Minute)
	other, err := foreign.Acquire(t.Context(), "endpoint:node-a/mock:slot:1", "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Supersede(t.Context(), old.ID, Spec{ID: "new", Kind: KindStep, Project: "p", Workspace: worktree("wt-new", "p"), Scope: ScopePathSet}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Check(t.Context(), other); err != nil {
		t.Fatalf("takeover invalidated another holder: %v", err)
	}
}

func TestRejectedCompletionClosesWithCandidateAndReleasesLeases(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		s, _ := newService(t)
		r := readyAttempt(t, s, "rejected")
		cause := errors.New("transaction rejected")
		want := Failed
		if conflict {
			cause, want = ledger.ErrConflict, BindConflict
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		completion := Completion{Result: Result{Artifact: "candidate", Summary: "finished work"}, Usage: &Usage{Input: 100, Reported: true}}
		if err := s.RejectCompletion(ctx, r.ID, "test", completion, cause); !errors.Is(err, cause) {
			t.Fatalf("original completion error lost: %v", err)
		}
		got, err := s.Get(t.Context(), r.ID)
		if err != nil || got.State != want || got.Result == nil || got.Result.Artifact != "candidate" || got.Usage == nil || got.Usage.Input != 100 {
			t.Fatalf("rejected completion not recorded: %+v, %v", got, err)
		}
		if live, err := s.Live(t.Context()); err != nil || len(live) != 0 {
			t.Fatalf("completed failure stayed live: %+v, %v", live, err)
		}
		for _, lease := range r.Leases {
			if err := s.l.Check(t.Context(), lease); !errors.Is(err, ledger.ErrStale) {
				t.Fatalf("rejected completion retained a lease: %v", err)
			}
		}
	}
}

func TestRejectedCompletionKeepsBothErrorsWhenCleanupCannotCommit(t *testing.T) {
	s, _ := newService(t)
	r := readyAttempt(t, s, "rejected")
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_failure BEFORE UPDATE ON operations WHEN NEW.state = 'failed' BEGIN SELECT RAISE(ABORT, 'cleanup rejected'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("completion rejected")
	err := s.RejectCompletion(t.Context(), r.ID, "test", Completion{Result: Result{Artifact: "candidate"}}, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "cleanup rejected") {
		t.Fatalf("cleanup swallowed an error: %v", err)
	}
	got, _ := s.Get(t.Context(), r.ID)
	if got.State != BindReady || got.Result != nil {
		t.Fatalf("failed cleanup partially changed state: %+v", got)
	}
	if err := s.l.Check(t.Context(), r.Leases[0]); err != nil {
		t.Fatalf("failed cleanup silently released a live record's lease: %v", err)
	}
}

package attempt

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func runningForAdmission(t *testing.T, s *Service, spec Spec) Record {
	t.Helper()
	r, err := s.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []State{Prepared, Running} {
		r, err = s.Advance(t.Context(), r.ID, phase, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestExpiredUnsweptWriterBlocksDirectAdmissionAndLanding(t *testing.T) {
	for _, conflict := range []string{"workspace", "endpoint", "task"} {
		t.Run(conflict, func(t *testing.T) {
			s, clock := newService(t)
			old := runningForAdmission(t, s, Spec{ID: "old", TaskID: "task", Node: "node", Harness: "mock", Slots: 2, Project: "p", Workspace: worktree("old", "p"), Scope: ScopePathSet})
			clock.t = clock.t.Add(2 * time.Minute)
			spec := Spec{ID: "new", TaskID: "other", Node: "other-node", Harness: "other", Project: "p", Workspace: worktree("new", "p"), Scope: ScopePathSet}
			switch conflict {
			case "workspace":
				spec.Workspace = old.Workspace
			case "endpoint":
				spec.Node, spec.Harness, spec.Slots = old.Node, old.Harness, 2
			case "task":
				spec.TaskID = old.TaskID
			}
			if _, err := s.Open(t.Context(), spec); !errors.Is(err, ErrStopConfirmationRequired) {
				t.Fatalf("direct Open after expiry without Sweep=%v", err)
			}
			if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckWriterTx(tx, old.Workspace.Node, old.Workspace.Path) }); !errors.Is(err, ErrStopConfirmationRequired) {
				t.Fatalf("landing admitted unswept writer: %v", err)
			}
		})
	}
}

func TestLiveParallelTasksAndSpareEndpointCapacityRemainUsable(t *testing.T) {
	s, clock := newService(t)
	old := runningForAdmission(t, s, Spec{ID: "one", TaskID: "parallel", Node: "node", Harness: "mock", Slots: 2, Project: "p", Workspace: worktree("one", "p"), Scope: ScopePathSet})
	clock.t = clock.t.Add(40 * time.Second)
	if err := s.Renew(t.Context(), old.ID); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(30 * time.Second) // persisted Lease.ExpiresAt is old; issuer is current
	two := runningForAdmission(t, s, Spec{ID: "two", TaskID: old.TaskID, Node: old.Node, Harness: old.Harness, Slots: 2, Project: "p", Workspace: worktree("two", "p"), Scope: ScopePathSet})
	if two.State != Running {
		t.Fatal("second free slot was serialized by task identity")
	}
}

func TestRunningAndArmRecheckWriterExpiryAfterOpen(t *testing.T) {
	for _, arm := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "arm"}[arm], func(t *testing.T) {
			s, clock := newService(t)
			old := runningForAdmission(t, s, Spec{ID: "old", TaskID: "same", Node: "one", Project: "p", Workspace: worktree("old", "p"), Scope: ScopePathSet})
			clock.t = clock.t.Add(40 * time.Second)
			next, err := s.Open(t.Context(), Spec{ID: "new", TaskID: old.TaskID, Node: "two", Project: "p", Workspace: worktree("new", "p"), Scope: ScopePathSet})
			if err != nil {
				t.Fatal(err)
			}
			next, err = s.Advance(t.Context(), next.ID, Prepared, "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			clock.t = clock.t.Add(30 * time.Second) // only the old own lease has expired
			if arm {
				err = s.ArmSession(t.Context(), next.ID, "new command")
			} else {
				_, err = s.Advance(t.Context(), next.ID, Running, "new prompt", nil)
			}
			if !errors.Is(err, ErrStopConfirmationRequired) {
				t.Fatalf("admission used pre-expiry snapshot: %v", err)
			}
			current, _ := s.Get(t.Context(), next.ID)
			if current.State != Prepared || current.SessionSettled == nil || !*current.SessionSettled {
				t.Fatalf("failed admission armed work: %+v", current)
			}
		})
	}
}

func TestPhysicalWriterBlocksAfterItsWorkspaceLeaseExpiresBeforeOwnLease(t *testing.T) {
	s, clock := newService(t)
	old := runningForAdmission(t, s, Spec{ID: "old", Project: "p", Workspace: canonical("p"), Scope: ScopeUnrestricted})
	clock.t = clock.t.Add(40 * time.Second)
	for _, lease := range old.Leases {
		if lease.Key == "attempt:"+old.ID {
			if _, err := s.l.Renew(t.Context(), lease, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	clock.t = clock.t.Add(30 * time.Second)
	if _, err := s.Open(t.Context(), Spec{ID: "new", Project: "p", Workspace: canonical("p"), Scope: ScopeUnrestricted}); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("renewal window admitted second physical writer: %v", err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckWriterTx(tx, "", old.Workspace.Path) }); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("renewal window admitted landing: %v", err)
	}
}

func TestPotentialWriterPreventsDeclarationReuseBeforeSweep(t *testing.T) {
	s, clock := newService(t)
	projects := project.Open(s.l, CheckDeclarationsTx)
	p := project.Project{ID: "p", Home: project.Home{Path: t.TempDir()}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	runningForAdmission(t, s, Spec{ID: "old", Project: "p", Workspace: project.Workspace{ID: "canonical:p", Project: "p", Kind: project.KindCanonical, Path: p.Home.Path}, Scope: ScopeUnrestricted})
	clock.t = clock.t.Add(2 * time.Minute)
	if err := projects.Reconcile(t.Context(), []project.Project{p}, "unchanged"); err != nil {
		t.Fatal(err)
	}
	if err := projects.Reconcile(t.Context(), []project.Project{{ID: "replacement", Home: p.Home}}, "reuse before sweep"); err == nil {
		t.Fatal("declaration reused active physical writer directory")
	}
}

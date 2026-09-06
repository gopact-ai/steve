package attempt

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartupRecoveryQuarantinesUnknownAndPreservesBoundOutput(t *testing.T) {
	s, _ := newService(t)
	open := func(id, taskID string) Record {
		r, err := s.Open(t.Context(), Spec{ID: id, TaskID: taskID, Kind: KindStep, Project: "p", Workspace: worktree("wt-"+id, "p"), Scope: ScopePathSet})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	idle := open("idle", "idle-task")
	old := open("old-prepared", "old-task")
	if _, err := s.Advance(t.Context(), old.ID, Prepared, "old-version", func(r *Record) { r.SessionSettled = nil }); err != nil {
		t.Fatal(err)
	}
	running := open("running", "running-task")
	for _, phase := range []State{Prepared, Running} {
		if _, err := s.Advance(t.Context(), running.ID, phase, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Advance(t.Context(), running.ID, Snapshotted, "test", func(r *Record) { r.Usage = &Usage{Reported: true, Input: 9} }); err != nil {
		t.Fatal(err)
	}
	settled := open("settled", "settled-task")
	for _, phase := range []State{Prepared, Running} {
		if _, err := s.Advance(t.Context(), settled.ID, phase, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSessionSettled(t.Context(), settled.ID, "confirmed response"); err != nil {
		t.Fatal(err)
	}
	bound := readyAttempt(t, s, "bound")
	body := []byte(`{"step":"work","answer":"done"}`)
	if _, err := s.Complete(t.Context(), bound.ID, "test", Completion{Result: Result{Artifact: "artifact", Output: body}, Usage: &Usage{Reported: true, Input: 4}, Binding: &NameBinding{Name: "done"}}); err != nil {
		t.Fatal(err)
	}
	report, err := s.PrepareRecovery(t.Context(), "startup")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Quarantined) != 2 || len(report.Expired) != 2 {
		t.Fatalf("startup report=%+v", report)
	}
	for _, id := range []string{idle.ID, settled.ID} {
		r, _ := s.Get(t.Context(), id)
		if r.State != Expired || r.Unsettled {
			t.Fatalf("safe idle retained: %+v", r)
		}
	}
	for _, id := range []string{old.ID, running.ID} {
		r, _ := s.Get(t.Context(), id)
		if !r.Unsettled || r.State == Expired {
			t.Fatalf("unknown writer released: %+v", r)
		}
	}
	saved, _ := s.Get(t.Context(), running.ID)
	if saved.Usage == nil || saved.Usage.Input != 9 {
		t.Fatal("startup overwrote observed spend")
	}
	saved, _ = s.Get(t.Context(), bound.ID)
	if saved.State != Bound || string(saved.Result.Output) != string(body) || saved.Usage.Input != 4 {
		t.Fatalf("startup damaged completed output: %+v", saved)
	}
	if _, err := s.Open(t.Context(), Spec{ID: "reroute", TaskID: running.TaskID, Node: "another", Harness: "another", Kind: KindStep, Project: "p", Workspace: worktree("new-place", "p"), Scope: ScopePathSet}); err == nil {
		t.Fatal("same-task rerouting bypassed unknown writer")
	}
	again, err := s.PrepareRecovery(t.Context(), "restart again")
	if err != nil || len(again.Quarantined) != 2 || len(again.Expired) != 0 {
		t.Fatalf("idempotent startup=%+v %v", again, err)
	}
}

func TestSessionEvidenceArmsBeforeWorkAndRequiresExplicitSettlement(t *testing.T) {
	s, _ := newService(t)
	r, err := s.Open(t.Context(), Spec{ID: "work", Kind: KindStep, Project: "p", Workspace: worktree("wt", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionSettled == nil || !*r.SessionSettled {
		t.Fatal("new unstarted attempt lacks safe evidence")
	}
	for _, phase := range []State{Prepared, Running} {
		r, err = s.Advance(t.Context(), r.ID, phase, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.SessionSettled == nil || *r.SessionSettled {
		t.Fatal("running was not armed durably")
	}
	if err := s.ReleaseEndpointAfterSessionClosed(t.Context(), r.ID, "session done"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(t.Context(), r.ID)
	if r.SessionSettled == nil || !*r.SessionSettled {
		t.Fatal("confirmed closure did not settle")
	}
	if err := s.ArmSession(t.Context(), r.ID, "command verifier"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(t.Context(), r.ID)
	if r.SessionSettled == nil || *r.SessionSettled {
		t.Fatal("verification reused stale settled proof")
	}
	if err := s.MarkUnsettled(t.Context(), r.ID, "test", errors.New("no exit evidence"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionSettled(t.Context(), r.ID, "mere EOF"); err == nil {
		t.Fatal("ordinary settlement silently cleared quarantine")
	}
}

func TestStartupRecoveryFailureDoesNotReleaseUnknownWriter(t *testing.T) {
	s, clock := newService(t)
	r := readyAttempt(t, s, "unknown")
	if err := s.ArmSession(t.Context(), r.ID, "verification started"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.DB().Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRecovery(t.Context(), "startup"); err == nil {
		t.Fatal("startup swallowed failed quarantine write")
	}
	if _, err := s.l.DB().Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(t.Context(), r.ID)
	if current.State != r.State {
		t.Fatal("failed startup mutation released attempt")
	}
	// Real leases still fence the unknown writer; merely opening a service
	// did not revoke them, and the next successful preparation excludes it.
	for _, lease := range r.Leases {
		if err := s.l.Check(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PrepareRecovery(t.Context(), "retry"); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Hour)
	if live, err := s.Live(context.Background()); err != nil || len(live) != 1 || !live[0].Unsettled {
		t.Fatalf("quarantine vanished: %+v %v", live, err)
	}
}

func TestSupersedeCannotTreatTTLAsStopEvidence(t *testing.T) {
	s, clock := newService(t)
	r, err := s.Open(t.Context(), Spec{ID: "old", TaskID: "task", Kind: KindStep, Project: "p", Workspace: worktree("wt-old", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []State{Prepared, Running} {
		if _, err := s.Advance(t.Context(), r.ID, phase, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	clock.t = clock.t.Add(time.Hour)
	_, err = s.Supersede(t.Context(), r.ID, Spec{ID: "new", TaskID: "task", Node: "elsewhere", Kind: KindStep, Project: "p", Workspace: worktree("wt-new", "p"), Scope: ScopePathSet}, "restart")
	if !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("unsafe takeover=%v", err)
	}
	current, _ := s.Get(t.Context(), r.ID)
	if !current.Unsettled || current.State != Running {
		t.Fatalf("unknown writer released: %+v", current)
	}
	if _, err := s.Get(t.Context(), "new"); err == nil {
		t.Fatal("replacement attempt started")
	}
}

func TestExpiryRechecksSettlementInsideItsTransaction(t *testing.T) {
	s, _ := newService(t)
	r := readyAttempt(t, s, "between-sessions")
	if r.SessionSettled == nil || !*r.SessionSettled {
		t.Fatal("fixture is not settled")
	}
	if err := s.ArmSession(t.Context(), r.ID, "verifier started after snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := s.expireSettled(t.Context(), r, "sweeper", "stale read"); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("expiry trusted stale safe state: %v", err)
	}
	current, _ := s.Get(t.Context(), r.ID)
	if current.State != r.State {
		t.Fatal("stale read expired active verifier")
	}
}

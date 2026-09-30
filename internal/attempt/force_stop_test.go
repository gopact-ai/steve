package attempt

import (
	"errors"
	"github.com/gopact-ai/steve/internal/task"
	"testing"
	"time"
)

func TestForceStopRequestsAreRevokedAndRevisionFenced(t *testing.T) {
	s, _, r, proof, tasks := retainedFixture(t)
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err == nil {
		t.Fatal("live task accepted force stop without revocation")
	}
	if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	first, err := s.RequestForceStop(t.Context(), r.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if first.ForceStop == nil || first.ForceStop.Level != "kill" || first.ForceStop.Revision != 1 || !first.Unsettled {
		t.Fatalf("request not recorded: %+v", first)
	}
	next, err := s.RequestForceStop(t.Context(), r.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if next.ForceStop.Revision != 2 {
		t.Fatal("repeated request did not fence earlier RPC")
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); !errors.Is(err, ErrForceStopChanged) {
		t.Fatalf("late result changed request: %v", err)
	}
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmForceStopped(t.Context(), r.ID, 1, proof); !errors.Is(err, ErrForceStopChanged) {
		t.Fatalf("late receipt changed request: %v", err)
	}
	stopped, err := s.ConfirmForceStopped(t.Context(), r.ID, 2, proof)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ForceStop.Level != "confirmed" || stopped.Unsettled || stopped.StopEvidence == "" {
		t.Fatalf("force stop was not confirmed atomically: %+v", stopped)
	}
}
func TestForceStopUnansweredPersistsTwoTriesAcrossRestart(t *testing.T) {
	s, now, r, _, tasks := retainedFixture(t)
	_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
	r, err := s.RequestForceStop(t.Context(), r.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.RecordForceStopResult(t.Context(), r.ID, 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.ForceStop.UnansweredCount != 1 {
		t.Fatal("first failed call not counted")
	}
	restarted := New(s.l)
	restarted.now = s.now
	now.t = now.t.Add(31 * time.Second)
	r, err = restarted.RecordForceStopResult(t.Context(), r.ID, 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.ForceStop.Level != "exhausted" || r.ForceStop.Reason != "restart_required" {
		t.Fatalf("unanswered stop not exhausted: %+v", r.ForceStop)
	}
}
func TestForceStopAnsweredResetsUnansweredAndBoundsRunning(t *testing.T) {
	s, now, r, _, tasks := retainedFixture(t)
	_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
	r, err := s.RequestForceStop(t.Context(), r.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.RecordForceStopResult(t.Context(), r.ID, 1, false, "")
	now.t = now.t.Add(29 * time.Second)
	r, err = s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_running")
	if err != nil {
		t.Fatal(err)
	}
	if !r.ForceStop.UnansweredSince.IsZero() || r.ForceStop.UnansweredCount != 0 || r.ForceStop.Level != "kill" {
		t.Fatalf("reply did not reset failure window: %+v", r.ForceStop)
	}
	before, _ := s.l.Events(t.Context(), r.ID)
	_, _ = s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_running")
	after, _ := s.l.Events(t.Context(), r.ID)
	if len(after) != len(before) {
		t.Fatal("identical polling result wrote another event")
	}
	now.t = now.t.Add(32 * time.Second)
	r, err = s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_running")
	if err != nil || r.ForceStop.Level != "exhausted" || r.ForceStop.Reason != "stop_running" {
		t.Fatalf("running limit=%+v %v", r.ForceStop, err)
	}
}
func TestForceStopUnprovableAndOldNodeExhaustWithoutAbort(t *testing.T) {
	for _, code := range []string{"stop_unproven", "stop_unsupported", "invalid", "unavailable", "forbidden"} {
		t.Run(code, func(t *testing.T) {
			s, _, r, _, tasks := retainedFixture(t)
			_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
			r, err := s.RequestForceStop(t.Context(), r.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.RecordForceStopResult(t.Context(), r.ID, 1, true, code)
			if err != nil || r.ForceStop.Level != "exhausted" {
				t.Fatalf("classification=%+v %v", r.ForceStop, err)
			}
			if code == "invalid" && r.ForceStop.Reason != "upgrade_required" {
				t.Fatal("old node did not ask for upgrade")
			}
		})
	}
}

func TestForceStopRequiresProcessExitNotJustAnAnsweredCommand(t *testing.T) {
	s, _, r, proof, tasks := retainedFixture(t)
	_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	proof.Session.Command.State = "cancelled"
	proof.Session.Command.Settled = true
	proof.Session.ProcessStopped = false
	if _, err := s.ConfirmForceStopped(t.Context(), r.ID, 1, proof); err == nil {
		t.Fatal("force stop accepted command settlement without process exit")
	}
	got, _ := s.Get(t.Context(), r.ID)
	if got.ForceStop.Level != "kill" || !got.Unsettled {
		t.Fatal("unproved force stop changed durable state")
	}
}

func TestOrdinaryStopCannotDowngradePendingForceStopProof(t *testing.T) {
	s, _, r, proof, tasks := retainedFixture(t)
	_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	proof.Session.Command.State = "cancelled"
	proof.Session.Command.Settled = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "earlier-stop", proof); err == nil {
		t.Fatal("earlier graceful stop confirmed an outstanding force stop without exit")
	}
	got, _ := s.Get(t.Context(), r.ID)
	if got.ForceStop.Level != "kill" || !got.Unsettled {
		t.Fatal("graceful receipt cleared force quarantine")
	}
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "earlier-stop", proof); err != nil {
		t.Fatal(err)
	}
}

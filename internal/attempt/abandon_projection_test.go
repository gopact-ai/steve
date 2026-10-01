package attempt

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func abandonedRecord(t *testing.T) (*Service, Record, RetainedEvidence, *task.Store) {
	t.Helper()
	s, _, r, proof, tasks := retainedFixture(t)
	if err := tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AbandonExecution(t.Context(), r.TaskID, r.ID, r.TurnID, func(tx *ledger.Tx, row task.Attempt, atTime time.Time) (task.RecoveryUsage, error) {
		return s.AbandonTx(tx, r.ID, "owner", 1, row, atTime)
	}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(t.Context(), r.ID)
	return s, r, proof, tasks
}

func TestAbandonAdmissionWaitsForProjectionAndKeepsThePhysicalWriter(t *testing.T) {
	s, r, _, _ := abandonedRecord(t)
	spec := r.Spec
	spec.ID, spec.TaskID, spec.Slots = "new", "new-task", 1
	spec.Workspace.ID, spec.Workspace.Path = "other", r.Workspace.Path+"-other"
	check := func() error {
		return s.l.Update(t.Context(), func(tx *ledger.Tx) error { return checkAdmissionTx(tx, spec) })
	}
	if !errors.Is(check(), ErrStopConfirmationRequired) {
		t.Fatal("unprojected abandonment released endpoint isolation")
	}
	projected, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("another directory remains blocked after abandonment: %v", err)
	}
	spec.Workspace.Path = r.Workspace.Path
	if !errors.Is(check(), ErrStopConfirmationRequired) {
		t.Fatal("abandonment allowed the old physical directory")
	}
	if !projected.Unsettled || projected.StopEvidence != "" {
		t.Fatal("capacity projection invented stop evidence")
	}
}

func TestAbandonedCommandReceiptCannotReleaseItsWriter(t *testing.T) {
	s, r, proof, _ := abandonedRecord(t)
	proof.Session.ProcessStopped = false
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "recheck", proof); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("command-only receipt accepted: %v", err)
	}
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "recheck", proof); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1)
	if err != nil || got.Abandoned.ProjectedAt.IsZero() {
		t.Fatalf("confirmed before projection lost obligation: %+v %v", got.Abandoned, err)
	}
}

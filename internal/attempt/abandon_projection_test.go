package attempt

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
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
		return s.AbandonTx(tx, r.ID, "owner", 1, row, atTime, AbandonContext{State: "absent"})
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
	proof.Session.Command.State = nodewire.SessionCommandCancelled
	proof.Session.Command.Settled = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "recheck", proof); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("command-only receipt accepted: %v", err)
	}
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "recheck", proof); err != nil {
		t.Fatal(err)
	}
	pending, err := s.AbandonProjections(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("confirmed execution lost pending projection: %v %v", pending, err)
	}
	got, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1)
	if err != nil || got.Abandoned.ProjectedAt.IsZero() {
		t.Fatalf("confirmed before projection lost obligation: %+v %v", got.Abandoned, err)
	}
}

func TestAbandonmentProjectionRemainsVisibleAfterPhysicalConfirmation(t *testing.T) {
	s, r, proof, _ := abandonedRecord(t)
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "node-exit", proof); err != nil {
		t.Fatal(err)
	}
	live, err := s.Live(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != r.ID || live[0].Abandoned == nil {
		t.Fatal("physical exit hid unfinished abandonment projection")
	}
	spec := r.Spec
	spec.ID = "replacement"
	spec.TaskID = "another-task"
	spec.Slots = 1
	spec.Workspace.ID = "other-directory"
	spec.Workspace.Path = r.Workspace.Path + "-other"
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return checkAdmissionTx(tx, spec) }); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("native exit bypassed pending session retirement: %v", err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckWriterTx(tx, r.Workspace.Node, r.Workspace.Path) }); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("writer admission bypassed pending session retirement: %v", err)
	}

	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_projection BEFORE UPDATE ON operations WHEN NEW.id='att-live' AND json_extract(NEW.data,'$.abandoned.projected_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'projection refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1); err == nil {
		t.Fatal("rejected projection returned success")
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return checkAdmissionTx(tx, spec) }); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatal("rejected projection removed admission barrier")
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(`DROP TRIGGER refuse_projection`); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectAbandonedCapacity(t.Context(), r.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return checkAdmissionTx(tx, spec) }); err != nil {
		t.Fatalf("completed retirement remained blocked: %v", err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckWriterTx(tx, r.Workspace.Node, r.Workspace.Path) }); err != nil {
		t.Fatalf("confirmed retired writer remained blocked: %v", err)
	}
	live, err = s.Live(t.Context())
	if err != nil || len(live) != 0 {
		t.Fatalf("projected confirmed abandonment stayed live: %v %v", live, err)
	}
}

func TestAbandonedWriterStillRequiresExitAfterALateQuarantineUpdate(t *testing.T) {
	s, r, proof, _ := abandonedRecord(t)
	proof.Session.ProcessStopped = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "physical-exit", proof); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsettled(t.Context(), r.ID, "late-observer", ErrStopConfirmationRequired, nil); err != nil {
		t.Fatal(err)
	}
	proof.Session.ProcessStopped = false
	proof.Session.Command.State = nodewire.SessionCommandCancelled
	proof.Session.Command.Settled = true
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "old-command", proof); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("abandoned writer accepted command-only evidence after a late update: %v", err)
	}
	proof.Session.ProcessStopped = true
	proof.Session.Binding.AttemptID = "other"
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "wrong-exit", proof); err == nil {
		t.Fatal("wrong process identity confirmed abandoned writer")
	}
	proof.Session.Binding.AttemptID = r.ID
	if _, err := s.ConfirmTaskStopped(t.Context(), r.ID, "fresh-exit", proof); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedWriterDoesNotAcceptADeclarativeStopInsteadOfNativeProof(t *testing.T) {
	s, r, _, _ := abandonedRecord(t)
	if _, err := s.ConfirmStopped(t.Context(), r.ID, "owner", "the writer is said to be stopped"); !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("abandoned writer accepted a declaration instead of a native receipt: %v", err)
	}
}

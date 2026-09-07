package attempt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func TestConfirmedTaskStopRequiresRevocationAndExactNativeEvidence(t *testing.T) {
	for _, changed := range []string{"live-task", "session", "command", "binding", "stale-proof", "unknown-stop", "settled", "process-exit"} {
		t.Run(changed, func(t *testing.T) {
			s, now, old, proof, tasks := retainedFixture(t)
			proof.Session.Command.State, proof.Session.Command.Settled = "cancelled", true
			proof.Session.State = "idle"
			if changed != "live-task" {
				if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
			}
			switch changed {
			case "session":
				proof.Session.ID = "ns_other"
			case "command":
				proof.Session.Command.ID = "other-input"
			case "binding":
				proof.Session.Binding.TaskEpoch++
			case "stale-proof":
				proof.ObservedAt = now.t.Add(-2 * time.Minute)
			case "unknown-stop":
				proof.Session.Command.Settled = false
			case "process-exit":
				proof.Session.Command = nil
				proof.Session.State, proof.Session.ProcessStopped = "closed", true
			}
			got, err := s.ConfirmTaskStopped(t.Context(), old.ID, "replacement-coordinator", proof)
			want := changed == "settled" || changed == "process-exit"
			if want != (err == nil) {
				t.Fatalf("stop %s: %+v %v", changed, got, err)
			}
			if !want {
				current, _ := s.Get(t.Context(), old.ID)
				if current.State != old.State || current.Revision != old.Revision {
					t.Fatal("rejected stop changed the original attempt")
				}
				if _, exists, err := s.TaskStopReceipt(t.Context(), old.ID); err != nil || exists {
					t.Fatalf("rejected stop leaked an accepted receipt: %v %v", exists, err)
				}
				return
			}
			if got.Unsettled || got.State != Failed || got.SessionSettled == nil || !*got.SessionSettled || !strings.HasPrefix(got.StopEvidence, "task-stop/") {
				t.Fatalf("native stop not committed: %+v", got)
			}
			before, _ := s.l.Events(t.Context(), old.ID)
			again, err := s.ConfirmTaskStopped(t.Context(), old.ID, "replacement-coordinator", proof)
			after, _ := s.l.Events(t.Context(), old.ID)
			if err != nil || again.Revision != got.Revision || len(after) != len(before) {
				t.Fatalf("identical stop receipt was not idempotent: %+v %v", again, err)
			}
			if err := s.Renew(t.Context(), old.ID); err == nil {
				t.Fatal("stopped attempt renewed its original writer lease")
			}
		})
	}
}

func TestTaskStopAfterResumeDoesNotUpgradeOrFinishTheNewTaskTurn(t *testing.T) {
	s, _, old, proof, tasks := retainedFixture(t)
	if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(old.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	proof.Session.State = "idle"
	proof.Session.Command.State, proof.Session.Command.Settled = "completed", true
	proof.Session.Progress.Usage.InputTokens, proof.Session.Progress.Usage.OutputTokens = 50, 20
	if err := s.MarkUnsettled(t.Context(), old.ID, "restart", errors.New("old observer gone"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmTaskStopped(t.Context(), old.ID, "replacement-coordinator", proof)
	if err != nil || got.Usage == nil || got.Usage.Input != 50 || got.Usage.Output != 20 {
		t.Fatalf("known stopped usage not retained: %+v %v", got, err)
	}
	current, _ := tasks.Get(old.TaskID)
	if current.State != task.StateRunning || !current.Attempts[len(current.Attempts)-1].Open() {
		t.Fatal("old native stop changed current task/turn")
	}
}

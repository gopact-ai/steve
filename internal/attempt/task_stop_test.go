package attempt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
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

// A stop still waiting on its node is recorded once, over whatever the
// attempt was quarantined for before: the owner reads that the pause or
// cancel is recorded. Checking again adds no event, even after the Hub
// changed language and so words the same explanation differently.
func TestTaskStopPendingIsRecordedOnce(t *testing.T) {
	zh := i18n.New(i18n.LocaleZH).T(i18n.AppStopPending)
	for _, quarantined := range []bool{false, true} {
		t.Run(fmt.Sprintf("quarantined=%t", quarantined), func(t *testing.T) {
			s, _, old, _, tasks := retainedFixture(t)
			if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
				t.Fatal(err)
			}
			if quarantined {
				if err := s.MarkUnsettled(t.Context(), old.ID, "restart", errors.New("old observer gone"), nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := taskStopPending(t, s, old.ID, i18n.LocaleZH); err != nil {
				t.Fatal(err)
			}
			first, _ := s.Get(t.Context(), old.ID)
			if !first.Unsettled || first.Error != zh {
				t.Fatalf("stop pending recorded as %q, want %q", first.Error, zh)
			}
			before, _ := s.l.Events(t.Context(), old.ID)
			if err := taskStopPending(t, s, old.ID, i18n.LocaleEN); err != nil {
				t.Fatal(err)
			}
			current, _ := s.Get(t.Context(), old.ID)
			after, _ := s.l.Events(t.Context(), old.ID)
			if !current.Unsettled || current.Error != zh || current.Revision != first.Revision || len(after) != len(before) {
				t.Fatalf("pending stop recorded again: %q revision %d->%d, events %d->%d", current.Error, first.Revision, current.Revision, len(before), len(after))
			}
		})
	}
}

// taskStopPending records a stop pending on its node as the Hub does
// while it speaks locale.
func taskStopPending(t *testing.T, s *Service, id string, locale i18n.Locale) error {
	return s.TaskStopPending(i18n.WithLocale(t.Context(), locale), id, "task-stop-recovery", i18n.New(locale).T(i18n.AppStopPending))
}

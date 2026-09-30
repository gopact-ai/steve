package attempt

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestProcessExitConvergesAnOutstandingForceStop(t *testing.T) {
	for _, level := range []string{"kill", "exhausted"} {
		t.Run(level, func(t *testing.T) {
			s, now, r, proof, tasks := retainedFixture(t)
			if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if level == "exhausted" {
				if _, err := s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
					t.Fatal(err)
				}
			}
			now.t = now.t.Add(time.Second)
			proof.ObservedAt = now.t
			proof.Session.ProcessStopped = true
			proof.Session.Command.ProcessStopped = true
			proof.Session.Command.Settled = false
			got, err := s.ConfirmProcessStopped(t.Context(), r.ID, "native-exit", proof)
			if err != nil {
				t.Fatal(err)
			}
			f := got.ForceStop
			if f == nil || f.Level != "confirmed" || !f.LevelSince.Equal(now.t) || f.Reason != "" || !f.ExhaustedAt.IsZero() || !f.UnansweredSince.IsZero() || f.UnansweredCount != 0 {
				t.Fatalf("valid exit left force state stale: %+v", f)
			}
			pending, err := s.StopCandidates(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatal("confirmed process requires another kill")
			}
		})
	}
}

func TestProcessExitCannotConfirmForceStopOnWrongOrRefusedProof(t *testing.T) {
	for _, failure := range []string{"binding", "write"} {
		t.Run(failure, func(t *testing.T) {
			s, _, r, proof, tasks := retainedFixture(t)
			_, _ = tasks.SetAside(r.TaskID, task.StateCancelled)
			if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			proof.Session.ProcessStopped = true
			if failure == "binding" {
				proof.Session.Binding.AttemptID = "another"
			} else {
				if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
					_, err := tx.Exec(`CREATE TRIGGER refuse_process_stop BEFORE UPDATE ON operations WHEN NEW.id='att-live' BEGIN SELECT RAISE(ABORT,'receipt refused'); END`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ConfirmProcessStopped(t.Context(), r.ID, "native-exit", proof); err == nil {
				t.Fatal("invalid proof was accepted")
			}
			got, _ := s.Get(t.Context(), r.ID)
			if !got.Unsettled || got.ForceStop.Level != "kill" {
				t.Fatal("rejected proof changed force state")
			}
		})
	}
}

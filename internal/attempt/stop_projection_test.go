package attempt

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

// A confirmed task stop is retired only after its accounting row, read in
// the same transaction, is closed with the usage the stop recorded.
func TestMarkStopProjectedWaitsForSettledAccounting(t *testing.T) {
	s, _ := newService(t)
	tasks, err := task.OpenLedger(s.l)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := tasks.Create(task.Task{Channel: "chat"})
	token, _ := tasks.ExecutionToken(root.ID)
	if _, err := tasks.ReserveAttempt(token, "stopped", "turn-1", "m1", "n1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	yes := true
	r := Record{Spec: Spec{ID: "stopped", TaskID: root.ID, TurnID: "turn-1", Kind: KindChat, Node: "n1", Execution: &token},
		State: Failed, Session: "ns_stopped", SessionSettled: &yes, StopEvidence: "task-stop/stopped", Revision: 1,
		Usage: &Usage{Input: 3, Output: 4, Model: "m", Reported: true}}
	putHistoryAttempt(t, s.l, r)

	requireMark := func(want bool) {
		t.Helper()
		got, err := s.MarkStopProjected(t.Context(), r.ID, "test")
		if err != nil || got != want {
			t.Fatalf("marked=%v err=%v want %v", got, err, want)
		}
		current, err := s.Get(t.Context(), r.ID)
		if err != nil || current.StopProjected != want || TaskStopOwed(current) == want {
			t.Fatalf("record=%+v err=%v", current, err)
		}
	}
	requireMark(false)
	if err := tasks.SettleAttempt(root.ID, r.ID, r.TurnID, time.Now(), task.OutcomeCancelled, task.RecoveryUsage{Tokens: task.Tokens{Input: 3}, Model: "m", Reported: true}); err != nil {
		t.Fatal(err)
	}
	requireMark(false)
	if err := tasks.SettleAttempt(root.ID, r.ID, r.TurnID, time.Now(), task.OutcomeCancelled, StoppedUsage(r)); err != nil {
		t.Fatal(err)
	}
	requireMark(true)
	requireMark(true)
}

func TestMarkStopProjectedRefusesRecordsThatAreNotConfirmedStops(t *testing.T) {
	s, _ := newService(t)
	yes := true
	r := Record{Spec: Spec{ID: "settled", TaskID: "t", TurnID: "turn", Kind: KindChat, Node: "n1", Execution: &task.ExecutionToken{TaskID: "t", Epoch: 1}},
		State: Bound, Session: "ns_settled", SessionSettled: &yes, StopEvidence: "process-stop/settled", Revision: 1}
	raw, _ := json.Marshal(r)
	insertAttemptRow(t, s, r.ID, string(r.State), string(raw))
	if marked, err := s.MarkStopProjected(t.Context(), r.ID, "test"); marked || err == nil {
		t.Fatalf("marked=%v err=%v", marked, err)
	}
}

// A projection mark belongs to the confirmation it was recorded for. When
// the stop is quarantined again and re-confirmed with other usage, a failed
// accounting settle must leave it a candidate so a later pass projects it.
func TestStopProjectionIsClearedWhenTheStopIsConfirmedAgain(t *testing.T) {
	s, now, old, proof, tasks := retainedFixture(t)
	if err := tasks.BindAttempt(*old.Execution, old.ID, old.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	proof.Session.State = "idle"
	proof.Session.Command.State, proof.Session.Command.Settled = "cancelled", true
	proof.Session.Progress.Usage.InputTokens = 5
	confirmed, err := s.ConfirmTaskStopped(t.Context(), old.ID, "stopper", proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SettleAttempt(old.TaskID, old.ID, old.TurnID, now.t, task.OutcomeCancelled, StoppedUsage(confirmed)); err != nil {
		t.Fatal(err)
	}
	if marked, err := s.MarkStopProjected(t.Context(), old.ID, "stopper"); err != nil || !marked {
		t.Fatalf("marked=%v err=%v", marked, err)
	}

	if err := s.MarkUnsettled(t.Context(), old.ID, "observer", errors.New("observer restarted"), nil); err != nil {
		t.Fatal(err)
	}
	proof.Session.Progress.Usage.InputTokens, proof.Session.Progress.Usage.OutputTokens = 9, 4
	reconfirmed, err := s.ConfirmTaskStopped(t.Context(), old.ID, "stopper", proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.DB().Exec(`CREATE TRIGGER reject_accounting BEFORE UPDATE ON bindings WHEN NEW.kind = 'task-attempt' BEGIN SELECT RAISE(FAIL, 'accounting unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SettleAttempt(old.TaskID, old.ID, old.TurnID, now.t, task.OutcomeCancelled, StoppedUsage(reconfirmed)); err == nil {
		t.Fatal("injected accounting failure did not fail the settle")
	}
	current, err := s.Get(t.Context(), old.ID)
	if err != nil || current.StopProjected || !TaskStopOwed(current) {
		t.Fatalf("re-confirmed stop kept its old projection: %+v %v", current, err)
	}
	if ids := stopCandidateIDs(t, s); !slices.Contains(ids, old.ID) {
		t.Fatalf("re-confirmed stop left the candidates: %v", ids)
	}
	if marked, err := s.MarkStopProjected(t.Context(), old.ID, "stopper"); err != nil || marked {
		t.Fatalf("stale accounting projected: marked=%v err=%v", marked, err)
	}

	if _, err := s.l.DB().Exec(`DROP TRIGGER reject_accounting`); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SettleAttempt(old.TaskID, old.ID, old.TurnID, now.t, task.OutcomeCancelled, StoppedUsage(reconfirmed)); err != nil {
		t.Fatal(err)
	}
	if marked, err := s.MarkStopProjected(t.Context(), old.ID, "stopper"); err != nil || !marked {
		t.Fatalf("retry did not project: marked=%v err=%v", marked, err)
	}
	if ids := stopCandidateIDs(t, s); slices.Contains(ids, old.ID) {
		t.Fatalf("projected stop still a candidate: %v", ids)
	}
}

func TestStopProjectionHoldsOnlyForTheSameConfirmation(t *testing.T) {
	yes := true
	confirmed := Record{Spec: Spec{ID: "a", TaskID: "t", Kind: KindChat, Node: "n1", Execution: &task.ExecutionToken{TaskID: "t", Epoch: 1}},
		State: Failed, Session: "ns_a", SessionSettled: &yes, StopEvidence: "task-stop/a", StopProjected: true, Usage: &Usage{Input: 2, Reported: true}}
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
		want   bool
	}{
		{"unchanged", func(*Record) {}, true},
		{"expired later", func(r *Record) { r.State = Expired }, true},
		{"quarantined", func(r *Record) { r.Unsettled = true }, false},
		{"usage changed", func(r *Record) { r.Usage = &Usage{Input: 3, Reported: true} }, false},
		{"usage dropped", func(r *Record) { r.Usage = nil }, false},
		{"other evidence", func(r *Record) { r.StopEvidence = "process-stop/a" }, false},
		{"superseded", func(r *Record) { r.State = Superseded }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := confirmed
			tc.mutate(&next)
			if got := stopProjectionHolds(confirmed, next); got != tc.want {
				t.Fatalf("holds=%v want %v", got, tc.want)
			}
		})
	}
}

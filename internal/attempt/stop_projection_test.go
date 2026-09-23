package attempt

import (
	"encoding/json"
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

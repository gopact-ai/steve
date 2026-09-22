package task

import (
	"path/filepath"
	"testing"
)

// A user's stop is the last word until they speak again: the hold outlives
// the process, so the start-up delivery pass cannot wake the task either,
// and it does not change what the children still owe.
func TestAHoldOutlivesTheProcessAndLeavesTheChildrensDebtAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	child, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b", Origin: "delegate:" + root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(child.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResult(child.ID, Result{Outcome: "ok", Answer: "did it"}); err != nil {
		t.Fatal(err)
	}
	held, err := s.Hold(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !held.Held() || held.State != root.State || held.ExecutionEpoch != root.ExecutionEpoch {
		t.Fatalf("held task = %+v; want the hold set and state/epoch untouched", held)
	}
	if len(s.Undelivered()[root.ID]) != 1 {
		t.Fatal("a hold changed what the child owes its parent")
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := again.Get(root.ID)
	if !reloaded.Held() {
		t.Fatal("a restart forgot the user's stop")
	}
	released, err := again.ReleaseHold(root.ID, reloaded.HeldAt)
	if err != nil {
		t.Fatal(err)
	}
	if released.Held() {
		t.Fatal("release left the hold in place")
	}
	if _, err := again.ReleaseHold(root.ID, reloaded.HeldAt); err != nil {
		t.Fatalf("releasing an unheld task: %v", err)
	}
	if _, err := s.Hold("no-such-task"); err == nil {
		t.Fatal("held a task that does not exist")
	}
}

// A release is bound to the stop it accounted for. A later stop stamps
// the hold again, and a turn composed under the earlier stamp — the one
// that stop cancelled — cannot lift it.
func TestALaterStopRestampsTheHoldSoTheEarlierTurnCannotLiftIt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	first, err := s.Hold(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Hold(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !second.HeldAt.After(first.HeldAt) {
		t.Fatalf("second stop stamped %v, first %v; want a later stamp", second.HeldAt, first.HeldAt)
	}
	stale, err := s.ReleaseHold(root.ID, first.HeldAt)
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Held() || !stale.HeldAt.Equal(second.HeldAt) {
		t.Fatalf("a release that saw the first stop lifted the second: %+v", stale)
	}
	if current, _ := s.ReleaseHold(root.ID, second.HeldAt); current.Held() {
		t.Fatal("the release that saw the second stop left it in place")
	}
}

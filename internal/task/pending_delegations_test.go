package task

import (
	"slices"
	"testing"
)

func TestPendingDelegationsListOnlyChildrenStillOwingTheirParent(t *testing.T) {
	s, err := OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	spawn := func(goal string) Task {
		t.Helper()
		child, err := s.Spawn(root.ID, Task{Goal: goal, Member: "b", Origin: "delegate:" + root.ID})
		if err != nil {
			t.Fatal(err)
		}
		return child
	}
	unstarted := spawn("unstarted")
	settled := spawn("settled")
	if _, err := s.Begin(settled.ID, "b", "n", "ns_settled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(settled.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(settled.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResult(settled.ID, Result{Outcome: "ok", Attempt: "att-settled"}); err != nil {
		t.Fatal(err)
	}
	openRow := spawn("result with open accounting")
	if _, err := s.Begin(openRow.ID, "b", "n", "ns_open"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResult(openRow.ID, Result{Outcome: "ok", Attempt: "att-open"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(openRow.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	noRows := spawn("result without accounting")
	if _, err := s.Advance(noRows.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResult(noRows.ID, Result{Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Spawn(root.ID, Task{Goal: "not delegated", Member: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Task{Goal: "orphan", Member: "b", Origin: "delegate:gone"}); err != nil {
		t.Fatal(err)
	}
	got := s.PendingDelegations()
	want := []string{unstarted.ID, openRow.ID, noRows.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("pending delegations = %v, want %v", got, want)
	}
	if stored, _ := s.Get(settled.ID); !stored.DelegationSettled() {
		t.Fatalf("settled child still owes its parent: %+v", stored)
	}
}

package task

import (
	"path/filepath"
	"testing"
)

func TestAChildsResultAndDeliveryOutliveTheProcess(t *testing.T) {
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
	if err := s.SetResult(child.ID, Result{Outcome: "ok", Answer: "did it", Refs: []string{"artifact abc"}, Attempt: "att-1"}); err != nil {
		t.Fatal(err)
	}
	waiting := s.Undelivered()
	if len(waiting[root.ID]) != 1 || waiting[root.ID][0].ID != child.ID {
		t.Fatalf("undelivered = %+v", waiting)
	}
	if err := s.SetDelivery(child.ID, DeliveryPending); err != nil {
		t.Fatal(err)
	}
	// Still owed after a pending mark; not after delivered.
	if len(s.Undelivered()[root.ID]) != 1 {
		t.Fatal("a pending delivery was forgotten")
	}
	if err := s.SetDelivery(child.ID, DeliveryDelivered); err != nil {
		t.Fatal(err)
	}
	if len(s.Undelivered()) != 0 {
		t.Fatalf("still owed: %+v", s.Undelivered())
	}
	// A new process reads the same facts back.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(child.ID)
	if got.Result == nil || got.Result.Answer != "did it" || got.Result.Refs[0] != "artifact abc" || got.Delivery == nil || got.Delivery.State != DeliveryDelivered || got.Delivery.Key != DeliveryKey(child.ID) {
		t.Fatalf("reloaded child = result %+v delivery %+v", got.Result, got.Delivery)
	}
	// What a reader gets is a copy.
	got.Result.Refs[0] = "tampered"
	if fresh, _ := again.Get(child.ID); fresh.Result.Refs[0] != "artifact abc" {
		t.Fatal("a returned task shares its result with the store")
	}
}

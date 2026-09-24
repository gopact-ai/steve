package task

import (
	"testing"
	"time"
)

func deliveryFixture(t *testing.T) (*Store, string, []string) {
	t.Helper()
	s, err := OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.Create(Task{Goal: "root", Channel: "console:test", Member: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(root.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, goal := range []string{"first", "second"} {
		child, err := s.Spawn(root.ID, Task{Goal: goal, Member: "b", Origin: "delegate:" + root.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Advance(child.ID, StateDone); err != nil {
			t.Fatal(err)
		}
		if err := s.SetResult(child.ID, Result{Outcome: OutcomeOK, Answer: goal}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, child.ID)
	}
	return s, root.ID, ids
}

func TestDeliveryBatchesPersistAtomicallyAndReturnCopies(t *testing.T) {
	s, parent, ids := deliveryFixture(t)
	gate := gateWrites(t, s)
	gate.fail = true
	if _, err := s.PrepareDeliveries(parent, ids); err == nil {
		t.Fatal("failed save acknowledged")
	}
	for _, id := range ids {
		got, _ := s.Get(id)
		if got.Delivery != nil {
			t.Fatal("partial batch installed")
		}
	}
	gate.fail = false
	batches, err := s.PrepareDeliveries(parent, append(ids, ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("batches = %+v", batches)
	}
	key := batches[0][0].Delivery.Key
	batches[0][0].Delivery.Key = "tampered"
	batches[0][1].Result.Answer = "tampered"
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*Store{s, reopened} {
		for _, id := range ids {
			got, _ := store.Get(id)
			if got.Delivery.Key != key || got.Result.Answer == "tampered" {
				t.Fatal("store leaked mutable batch")
			}
		}
	}
}

func TestInterruptedDeliveryRetryDependsOnChannelReceiptContract(t *testing.T) {
	for _, safe := range []bool{false, true} {
		s, parent, ids := deliveryFixture(t)
		if _, err := s.PrepareDeliveries(parent, ids); err != nil {
			t.Fatal(err)
		}
		if err := s.StartDelivery(ids, safe); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenLedger(s.book)
		if err != nil {
			t.Fatal(err)
		}
		expected := DeliveryUncertain
		if safe {
			expected = DeliveryPending
		}
		for _, child := range reopened.Undelivered()[parent] {
			if child.Delivery.State != expected || child.Delivery.Attempts != 1 {
				t.Fatalf("reloaded send = %+v", child.Delivery)
			}
		}
	}
}

func TestDeliveryOutcomeSaveFailureKeepsTheWholeBatchUnconfirmed(t *testing.T) {
	s, parent, ids := deliveryFixture(t)
	if _, err := s.PrepareDeliveries(parent, ids); err != nil {
		t.Fatal(err)
	}
	if err := s.StartDelivery(ids, true); err != nil {
		t.Fatal(err)
	}
	gateWrites(t, s).fail = true
	if err := s.RecordDelivery(ids, DeliveryDelivered, ""); err == nil {
		t.Fatal("failed receipt save acknowledged")
	}
	for _, id := range ids {
		got, _ := s.Get(id)
		if got.Delivery.State != DeliveryPending {
			t.Fatal("partial receipt installed")
		}
	}
}

func TestDeliveryChangeNotifiesWithoutChangingTaskElapsedTime(t *testing.T) {
	s, parent, ids := deliveryFixture(t)
	before, _ := s.Get(ids[0])
	observed := make(chan string, 8)
	s.SetObserver(func(id string) { observed <- id })
	if _, err := s.PrepareDeliveries(parent, ids); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("delivery did not notify read model")
	}
	after, _ := s.Get(ids[0])
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("delivery changed task elapsed time")
	}
}

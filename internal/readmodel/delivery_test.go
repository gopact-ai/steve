package readmodel

import (
	"github.com/gopact-ai/steve/internal/task"
	"testing"
)

func TestResultDeliveryRollsUpWithoutCountingSuccessAsAttention(t *testing.T) {
	list := []task.Task{
		{ID: "root", State: task.StateRunning},
		{ID: "child", Parent: "root", Origin: "delegate:root", State: task.StateDone, Result: &task.Result{Answer: "result"}},
		{ID: "uncertain", Parent: "child", Origin: "delegate:child", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryUncertain, Error: "receipt lost", Attempts: 2}},
		{ID: "delivered", Parent: "root", Origin: "delegate:root", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}},
		{ID: "suppressed", Parent: "root", Origin: "delegate:root", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliverySuppressed}},
	}
	b := snapshotBuilder{snap: Snapshot{Tasks: tasks(list, nil)}, activityKnown: true, attentionKnown: true}
	b.taskAxes()
	root := b.snap.Tasks[0]
	if root.PendingResults != 2 || root.UncertainResults != 1 || root.Attention != 0 || root.Lane != "needs_you" {
		t.Fatalf("root axes = %+v", root)
	}
	uncertain := b.snap.Tasks[2].ResultDelivery
	if uncertain.Error != "receipt lost" || uncertain.Attempts != 2 {
		t.Fatalf("missing delivery evidence: %+v", uncertain)
	}
	uncertain.Error = "tampered"
	if list[2].Delivery.Error != "receipt lost" {
		t.Fatal("projection shares mutable state")
	}
	list[2].Delivery.State = task.DeliveryDelivered
	b.snap.Tasks = tasks(list, nil)
	b.taskAxes()
	if b.snap.Tasks[0].Lane != "pending" || b.snap.Tasks[0].PendingResults != 1 {
		t.Fatalf("receipt did not clear attention: %+v", b.snap.Tasks[0])
	}
}

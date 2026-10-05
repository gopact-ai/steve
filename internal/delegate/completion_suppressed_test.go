package delegate

import (
	"context"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestRootCompletionAcceptsDurablySuppressedGrandchild(t *testing.T) {
	for _, end := range []task.State{task.StateDone, task.StateCancelled} {
		for _, route := range []string{"late-result", "queued-receipt"} {
			t.Run(string(end)+"/"+route, func(t *testing.T) {
				checkSuppressedGrandchildCompletion(t, end, route)
			})
		}
	}
}

func checkSuppressedGrandchildCompletion(t *testing.T, end task.State, route string) {
	t.Helper()
	w := newWorld(t)
	book := testLedger(t)
	var err error
	w.tasks, err = task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	w.service.tasks = w.tasks
	root := w.running(t, "codex")
	if _, err := w.tasks.Finish(root.ID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	child, err := w.tasks.Spawn(root.ID, task.Task{Member: "builder", Origin: "delegate:" + root.ID})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := w.tasks.Spawn(child.ID, task.Task{Member: "shipper", Origin: "delegate:" + child.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []task.Task{child, grandchild} {
		if _, err := w.tasks.Begin(member.ID, member.Member, "hub", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := w.tasks.Finish(member.ID, task.OutcomeOK, task.Tokens{Total: 7}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.tasks.SetResult(child.ID, task.Result{Outcome: task.OutcomeOK, Answer: "child finished"}); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetDelivery(child.ID, task.DeliveryDelivered); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(grandchild.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(grandchild.ID, task.Result{Outcome: task.OutcomeOK, Answer: "late result", Refs: []string{"artifact final"}}); err != nil {
		t.Fatal(err)
	}
	if route == "queued-receipt" {
		w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
		w.service.Flush(t.Context(), child.ID)
		queued, _ := w.tasks.Get(grandchild.ID)
		if queued.Delivery == nil || queued.Delivery.State != task.DeliveryQueued {
			t.Fatalf("result was not queued before the parent ended: %+v", queued.Delivery)
		}
		w.service.SetDeliveryReceipt(func(parent task.Task, key string) (bool, error) {
			if parent.ID != child.ID || key != queued.Delivery.Key {
				t.Fatal("receipt lookup changed the parent or delivery identity")
			}
			return true, channel.ErrDeliveryQueued
		})
	}
	if _, err := w.tasks.Advance(child.ID, end); err != nil {
		t.Fatal(err)
	}
	w.service.SetDeliverer(func(context.Context, Delivery) error {
		t.Fatal("an ended child must not receive a continuation")
		return nil
	})
	w.service.Flush(t.Context(), child.ID)

	// Completion must accept the durable suppression after a restart too,
	// without turning it into a delivery receipt or discarding the result.
	reloaded, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	root, _ = reloaded.Get(root.ID)
	before, _ := reloaded.Get(grandchild.ID)
	if before.Delivery == nil || before.Delivery.State != task.DeliverySuppressed || before.Delivery.Error == "" {
		t.Fatalf("flush did not persist suppression: %+v", before.Delivery)
	}
	if err := task.CompletionBlocker(root, reloaded.List(root.Channel)); err != nil {
		t.Errorf("durably suppressed descendant still blocks completion: %v", err)
	}
	if !task.CompletionEligibility(reloaded.List(root.Channel))[root.ID] {
		t.Error("durably suppressed descendant still blocks eligibility")
	}
	closed, err := reloaded.CompleteRoot(t.Context(), root.ID, root.Channel, func(tx *ledger.Tx, ids map[string]bool) error {
		if !ids[child.ID] || !ids[grandchild.ID] {
			t.Fatal("completion guard lost descendants")
		}
		if err := attempt.CheckTaskCompletionTx(tx, ids); err != nil {
			return err
		}
		return artifact.CheckTaskLandingsTx(tx, ids)
	})
	if err != nil || !closed.CompletedByUser {
		t.Fatalf("complete root after real suppression: %+v, %v", closed, err)
	}
	after, _ := reloaded.Get(grandchild.ID)
	if !reflect.DeepEqual(before.Result, after.Result) || !reflect.DeepEqual(before.Delivery, after.Delivery) || !reflect.DeepEqual(before.Attempts, after.Attempts) {
		t.Fatal("completion rewrote the suppressed result, receipt or accounting")
	}
}

package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

func completedChild(t *testing.T, w *world, parent task.Task, goal string) task.Task {
	t.Helper()
	child, err := w.tasks.Create(task.Task{Goal: goal, Parent: parent.ID, Channel: parent.Channel, Member: "builder", Origin: "delegate:" + parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(child.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(child.ID, task.Result{Outcome: task.OutcomeOK, Answer: goal}); err != nil {
		t.Fatal(err)
	}
	return child
}

func TestInlineCancelledChildIsNotDeliveredAgain(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	tracked, err := w.tasks.Create(task.Task{Parent: parent.ID, Channel: parent.Channel, Member: "builder", Origin: "delegate:" + parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(tracked.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(tracked.ID, task.Result{Outcome: task.OutcomeCancelled, Answer: "cancelled by owner"}); err != nil {
		t.Fatal(err)
	}
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	entry := &child{done: make(chan struct{}), started: time.Now(), result: agentmcp.DelegateResult{TaskID: tracked.ID, State: task.StateCancelled}}
	close(entry.done)
	if _, err := w.service.wait(t.Context(), entry, -1); err != nil {
		t.Fatal(err)
	}
	w.service.RedeliverPending(t.Context())
	if box.count() != 0 {
		t.Fatal("inline cancelled result was delivered again")
	}
	stored, _ := w.tasks.Get(tracked.ID)
	if stored.Delivery == nil || stored.Delivery.State != task.DeliveryDelivered {
		t.Fatal("inline result consumption was not durable")
	}
}

func TestPausedParentKeepsUnreportedResultsUntilResumed(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "implementation ready")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	if _, err := w.tasks.Advance(parent.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	w.service.RedeliverPending(t.Context())
	if box.count() != 0 {
		t.Fatal("paused parent was resumed")
	}
	if got := w.tasks.Undelivered()[parent.ID]; len(got) != 1 || got[0].ID != child.ID {
		t.Fatalf("paused parent lost its unreported result: %+v", got)
	}
	if _, err := w.tasks.Advance(parent.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	w.service.RedeliverPending(t.Context())
	if box.count() != 1 {
		t.Fatalf("resumed parent received %d results", box.count())
	}
}

func TestDeliveryReceiptLossDoesNotAbsorbALaterChild(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	first := completedChild(t, w, parent, "first result")
	accepted := map[string]Delivery{}
	loseReceipt := true
	w.service.SetDeliverer(func(_ context.Context, d Delivery) error {
		if _, ok := accepted[d.Key]; !ok {
			accepted[d.Key] = d
		}
		if loseReceipt {
			return errors.New("accepted, but reply was lost")
		}
		return nil
	})
	w.service.Flush(t.Context(), parent.ID)
	second := completedChild(t, w, parent, "later result")
	loseReceipt = false
	w.service.reconcileDeliveries(t.Context(), time.Now().Add(time.Hour))
	seen := map[string]int{}
	for _, d := range accepted {
		for _, child := range d.Children {
			seen[child.Task]++
		}
	}
	if seen[first.ID] != 1 || seen[second.ID] != 1 {
		t.Fatalf("accepted child results = %v; first=%s later=%s", seen, first.ID, second.ID)
	}
	if pending := w.tasks.Undelivered(); len(pending) != 0 {
		t.Fatalf("results still pending: %+v", pending)
	}
}

func TestReconcilerHonorsBackoffForEachBatch(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	first := completedChild(t, w, parent, "first")
	calls := 0
	w.service.SetDeliverer(func(context.Context, Delivery) error { calls++; return errors.New("unavailable") })
	w.service.Flush(t.Context(), parent.ID)
	pending, _ := w.tasks.Get(first.ID)
	due := pending.Delivery.NextAttemptAt
	completedChild(t, w, parent, "later")
	w.service.reconcileDeliveries(t.Context(), due.Add(-time.Nanosecond))
	if calls != 2 {
		t.Fatalf("fresh result should send without retrying earlier batch: calls=%d", calls)
	}
	w.service.reconcileDeliveries(t.Context(), due)
	if calls != 3 {
		t.Fatalf("only first batch should be due: calls=%d", calls)
	}
	retried, _ := w.tasks.Get(first.ID)
	if retried.Delivery.Attempts != 2 || retried.Delivery.NextAttemptAt.Sub(retried.Delivery.At) != 10*time.Second {
		t.Fatalf("retry record = %+v", retried.Delivery)
	}
}

func TestUncertainDeliveryIsNeverAutomaticallyReplayed(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "result")
	calls := 0
	w.service.SetDeliverer(func(context.Context, Delivery) error { calls++; return channel.ErrOutcomeUnknown })
	w.service.Flush(t.Context(), parent.ID)
	completedChild(t, w, parent, "later")
	w.service.RedeliverPending(t.Context())
	w.service.reconcileDeliveries(t.Context(), time.Now().Add(time.Hour))
	w.service.Flush(t.Context(), parent.ID)
	if calls != 2 {
		t.Fatalf("uncertain send was replayed: %d calls", calls)
	}
	got, _ := w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryUncertain || got.Delivery.Error == "" {
		t.Fatalf("missing uncertainty: %+v", got.Delivery)
	}
}

func TestAsynchronousReceiptCanArriveBeforeDispatchReturns(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "result")
	w.service.SetDeliverer(func(_ context.Context, d Delivery) error {
		w.service.ConfirmDelivery(d, nil)
		return channel.ErrOutcomeUnknown
	})
	w.service.Flush(t.Context(), parent.ID)
	got, _ := w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryDelivered {
		t.Fatalf("receipt was overwritten: %+v", got.Delivery)
	}
}

func TestFailedParentRetainsChildResult(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	completedChild(t, w, parent, "result")
	if _, err := w.tasks.Advance(parent.ID, task.StateFailed); err != nil {
		t.Fatal(err)
	}
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	w.service.ReconcileDeliveries(t.Context())
	if box.count() != 0 || len(w.tasks.Undelivered()[parent.ID]) != 1 {
		t.Fatal("failed parent lost or consumed result")
	}
}

func TestCancelledResultKeepsItsStateInParentDelivery(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	child, err := w.tasks.Create(task.Task{Parent: parent.ID, Channel: parent.Channel, Member: "builder", Origin: "delegate:" + parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(child.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(child.ID, task.Result{Outcome: task.OutcomeCancelled, Answer: "cancelled by owner"}); err != nil {
		t.Fatal(err)
	}
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	w.service.Flush(t.Context(), parent.ID)
	got := box.wait(t, 1)[0]
	if got.Children[0].State != task.StateCancelled || !strings.Contains(got.Notice(), "已取消") || !strings.Contains(got.Prompt(), "已取消") {
		t.Fatalf("cancellation became another outcome: %+v", got)
	}
}

package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

type continuationProcessor func(context.Context, turn.Request) (turn.Result, error)

func (f continuationProcessor) Handle(ctx context.Context, r turn.Request) (turn.Result, error) {
	return f(ctx, r)
}

func TestPauseBetweenBatchPreparationAndQueueAdmissionRetainsTheAnswer(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "unique child answer")
	processed := make(chan string, 2)
	cons := console.New(continuationProcessor(func(_ context.Context, r turn.Request) (turn.Result, error) {
		p, _ := w.tasks.Get(parent.ID)
		if p.State != task.StateRunning {
			return turn.Result{}, task.ErrContinuationUnavailable
		}
		processed <- r.Input
		return turn.Result{Text: "parent consumed child result"}, nil
	}), "owner", nil)
	paused := false
	w.service.SetReplaySafeDelivery(func(task.Task) bool { return true })
	w.service.SetDeliveryReceipt(func(p task.Task, key string) (bool, error) {
		return cons.ContinuationReceipt("main", p.ID, key)
	})
	w.service.SetDeliverer(func(ctx context.Context, d Delivery) error {
		if !paused {
			paused = true
			if _, err := w.tasks.Advance(parent.ID, task.StatePaused); err != nil {
				return err
			}
		}
		return cons.ContinueTask(ctx, "main", d.ParentTask, d.Key, d.Member, d.Notice(), d.Prompt())
	})
	waitSettled := func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			q := cons.Queue("main")
			if len(q) == 1 && q[0].State.Terminal() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("continuation did not settle")
	}
	w.service.Flush(t.Context(), parent.ID)
	waitSettled()
	w.service.ReconcileDeliveries(t.Context())
	got, _ := w.tasks.Get(child.ID)
	if got.Delivery.State == task.DeliveryDelivered {
		t.Fatal("paused parent never received the child answer but result was acknowledged")
	}
	if _, err := w.tasks.Advance(parent.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	w.service.reconcileDeliveries(t.Context(), time.Now().Add(time.Hour))
	select {
	case prompt := <-processed:
		if !strings.Contains(prompt, "unique child answer") {
			t.Fatal("full answer was lost")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not deliver retained answer")
	}
	waitSettled()
	if _, err := w.tasks.Advance(parent.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	w.service.reconcileDeliveries(t.Context(), time.Now().Add(time.Hour))
	got, _ = w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryDelivered {
		t.Fatalf("parent receipt missing: %+v", got.Delivery)
	}
	if got.Delivery.Attempts != 1 {
		t.Fatalf("queue receipt inspection inflated send attempts: %d", got.Delivery.Attempts)
	}
	if len(cons.Queue("main")) != 1 {
		t.Fatal("resume created a duplicate queue entry")
	}
}

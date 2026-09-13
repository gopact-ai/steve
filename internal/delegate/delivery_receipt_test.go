package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

func TestReceiptSettlesUncertainOrDelayedDeliveryWithoutDispatch(t *testing.T) {
	for _, outcome := range []string{task.DeliveryUncertain, task.DeliveryPending} {
		t.Run(outcome, func(t *testing.T) {
			w := newWorld(t)
			parent := w.running(t, "codex")
			child := completedChild(t, w, parent, "retained answer")
			ids := []string{child.ID}
			if _, err := w.tasks.PrepareDeliveries(parent.ID, ids); err != nil {
				t.Fatal(err)
			}
			if err := w.tasks.StartDelivery(ids, true); err != nil {
				t.Fatal(err)
			}
			if err := w.tasks.RecordDelivery(ids, outcome, "lost response"); err != nil {
				t.Fatal(err)
			}
			if _, err := w.tasks.Advance(parent.ID, task.StatePaused); err != nil {
				t.Fatal(err)
			}
			w.service.SetDeliverer(func(context.Context, Delivery) error { t.Fatal("receipt caused a dispatch"); return nil })
			w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { return true, nil })
			w.service.reconcileDeliveries(t.Context(), time.Now())
			got, _ := w.tasks.Get(child.ID)
			if got.Delivery.State != task.DeliveryDelivered || got.Delivery.Attempts != 1 {
				t.Fatalf("receipt did not settle original dispatch: %+v", got.Delivery)
			}
		})
	}
}

func TestReceiptSaveFailureRetainsBatchAcrossRestartWithoutDispatch(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tasks.json")
	var err error
	w.tasks, err = task.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w.service.tasks = w.tasks
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "durable parent answer")
	w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
	w.service.Flush(t.Context(), parent.ID)
	lookups := 0
	w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { lookups++; return true, nil })
	w.service.SetDeliverer(func(context.Context, Delivery) error { t.Fatal("observed receipt was dispatched again"); return nil })
	// Replace the private fixture's directory with a file to fail the next
	// atomic task write while preserving the previous durable document.
	if err := os.Rename(dir, dir+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("unavailable storage"), 0600); err != nil {
		t.Fatal(err)
	}
	w.service.Flush(t.Context(), parent.ID)
	got, _ := w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryQueued || lookups != 1 {
		t.Fatalf("failed save changed the receipt: %+v; lookups=%d", got.Delivery, lookups)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+"-saved", dir); err != nil {
		t.Fatal(err)
	}
	w.tasks, err = task.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w.service.tasks = w.tasks
	w.service.Flush(t.Context(), parent.ID)
	got, _ = w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryDelivered || got.Delivery.Attempts != 1 || lookups != 2 {
		t.Fatalf("restart lost the repeatable receipt: %+v; lookups=%d", got.Delivery, lookups)
	}
}

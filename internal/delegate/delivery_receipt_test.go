package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
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
	book := taskBook(t)
	var err error
	w.tasks, err = task.OpenLedger(book)
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
	gate := &refusingReplicator{book: book, refuse: true}
	if err := book.AttachReplication(gate); err != nil {
		t.Fatal(err)
	}
	w.service.Flush(t.Context(), parent.ID)
	got, _ := w.tasks.Get(child.ID)
	if got.Delivery.State != task.DeliveryQueued || lookups != 1 {
		t.Fatalf("failed save changed the receipt: %+v; lookups=%d", got.Delivery, lookups)
	}
	gate.refuse = false
	w.tasks, err = task.OpenLedger(book)
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

// refusingReplicator stands in for ledger replication so a test can refuse
// durable writes while the previous committed state stays readable.
type refusingReplicator struct {
	book   *ledger.Ledger
	refuse bool
}

func (r *refusingReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *refusingReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	if r.refuse {
		return nil, errors.New("storage unavailable")
	}
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

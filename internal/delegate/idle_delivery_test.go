package delegate

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// A result the parent's conversation has queued, and not yet taken up,
// is looked at on every reconciliation pass. Finding it still queued
// changes nothing, so an idle pass writes nothing: not the child's
// record, and no write transaction either.
func TestAContinuationStillQueuedIsNotRewrittenEachPass(t *testing.T) {
	w := newWorld(t)
	book := testLedger(t)
	var err error
	w.tasks, err = task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	w.service.tasks = w.tasks
	parent := w.running(t, "codex")
	child := completedChild(t, w, parent, "waiting on the owner")
	w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
	w.service.Flush(t.Context(), parent.ID)
	queued, _ := w.tasks.Get(child.ID)
	if queued.Delivery == nil || queued.Delivery.State != task.DeliveryQueued {
		t.Fatalf("delivery = %+v, want queued", queued.Delivery)
	}
	w.service.SetDeliverer(func(context.Context, Delivery) error { t.Fatal("a queued continuation was sent again"); return nil })
	w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { return true, channel.ErrDeliveryQueued })
	writes := &countingReplicator{book: book}
	if err := book.AttachReplication(writes); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for pass := range 3 {
		w.service.reconcileDeliveries(t.Context(), now.Add(time.Duration(pass)*5*time.Second))
	}
	if writes.prepared != 0 || writes.proposed != 0 {
		t.Errorf("three idle passes opened %d write transactions and committed %d", writes.prepared, writes.proposed)
	}
	if got, _ := w.tasks.Get(child.ID); !reflect.DeepEqual(got.Delivery, queued.Delivery) {
		t.Errorf("delivery changed from %+v to %+v", queued.Delivery, got.Delivery)
	}
}

// A parent that has ended takes nothing more. A continuation its
// conversation still holds unprocessed, as one waiting on the owner's
// answer does, no longer keeps the child queued: the child is suppressed,
// says why, and is left alone by later passes.
func TestAnEndedParentSuppressesAContinuationItsConversationStillHolds(t *testing.T) {
	for _, end := range []task.State{task.StateDone, task.StateCancelled} {
		t.Run(string(end), func(t *testing.T) {
			w := newWorld(t)
			parent := w.running(t, "codex")
			child := completedChild(t, w, parent, "waiting on the owner")
			w.service.SetDeliverer(func(context.Context, Delivery) error { return channel.ErrDeliveryQueued })
			w.service.Flush(t.Context(), parent.ID)
			if _, err := w.tasks.Advance(parent.ID, end); err != nil {
				t.Fatal(err)
			}
			w.service.SetDeliverer(func(context.Context, Delivery) error { t.Error("an ended parent was sent a continuation"); return nil })
			lookups := 0
			w.service.SetDeliveryReceipt(func(task.Task, string) (bool, error) { lookups++; return true, channel.ErrDeliveryQueued })
			now := time.Now()
			w.service.reconcileDeliveries(t.Context(), now)
			got, _ := w.tasks.Get(child.ID)
			if got.Delivery == nil || got.Delivery.State != task.DeliverySuppressed {
				t.Fatalf("delivery = %+v, want suppressed", got.Delivery)
			}
			for _, want := range []string{"#" + parent.ID, string(end), got.Delivery.Key} {
				if !strings.Contains(got.Delivery.Error, want) {
					t.Errorf("reason %q does not name %q", got.Delivery.Error, want)
				}
			}
			settled := *got.Delivery
			for pass := 1; pass <= 2; pass++ {
				w.service.reconcileDeliveries(t.Context(), now.Add(time.Duration(pass)*5*time.Second))
			}
			if lookups != 1 {
				t.Errorf("receipt looked up %d times, want once", lookups)
			}
			if got, _ := w.tasks.Get(child.ID); got.Delivery == nil || !reflect.DeepEqual(*got.Delivery, settled) {
				t.Errorf("delivery changed from %+v to %+v", settled, got.Delivery)
			}
		})
	}
}

// countingReplicator stands in for ledger replication and counts what the
// ledger asks of it.
type countingReplicator struct {
	book               *ledger.Ledger
	prepared, proposed int
}

func (r *countingReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	r.prepared++
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *countingReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	r.proposed++
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

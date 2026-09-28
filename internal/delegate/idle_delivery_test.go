package delegate

import (
	"context"
	"reflect"
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
	// The parent may end while its conversation still holds the queued
	// continuation, as one waiting on the owner's answer does.
	if _, err := w.tasks.Advance(parent.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
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

package artifact

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// The background sweep asks every project for queued results every pass.
// A project with nothing queued answers from a read: no driver lease is
// taken or released, so an idle pass sends nothing to the ledger. A result
// queued afterwards still lands on the next pass.
func TestASweepWithNothingQueuedTakesNoLease(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	writes := attachCounting(t, store.ledger)

	landed, err := store.LandPending(ctx, p)
	if err != nil || len(landed) != 0 {
		t.Fatalf("idle pass = %+v err=%v", landed, err)
	}
	if n := writes.prepared.Load(); n != 0 {
		t.Errorf("an idle pass opened %d ledger writes", n)
	}

	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	write(t, ws.Path, "a", "a1")
	result, _, err := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Defer(ctx, "p", result.ID, "att-1"); err != nil {
		t.Fatal(err)
	}
	landed, err = store.LandPending(ctx, p)
	if err != nil || len(landed) != 1 || landed[0].State != LandCommitted || read(t, canonical, "a") != "a1" {
		t.Fatalf("queued result = %+v err=%v", landed, err)
	}
}

// A result stuck on a conflict that the canonical has not moved past is
// skipped by every pass. Skipping it is decided from reads, so a project
// whose only queued result is stuck costs the ledger nothing per pass,
// whether the canonical stayed or moved on paths the conflict does not touch.
func TestAStuckResultTakesNoLeaseEachSweep(t *testing.T) {
	ctx := t.Context()
	store, p, _, canonical, _ := applyConflicted(t)
	writes := attachCounting(t, store.ledger)

	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("stuck pass = %+v err=%v", again, err)
	}
	if n := writes.prepared.Load(); n != 0 {
		t.Errorf("a pass over a stuck result opened %d ledger writes", n)
	}

	write(t, canonical, "other", "moved")
	if _, _, err := store.SnapshotCanonical(ctx, p, canonicalOf(t, store, "p"), "user", "moved"); err != nil {
		t.Fatal(err)
	}
	writes.prepared.Store(0)
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("pass after an unrelated move = %+v err=%v", again, err)
	}
	if n := writes.prepared.Load(); n != 0 {
		t.Errorf("a pass over a stuck result after an unrelated move opened %d ledger writes", n)
	}
}

// countingReplicator stands in for ledger replication and counts the write
// transactions the ledger opens.
type countingReplicator struct {
	book     *ledger.Ledger
	prepared atomic.Int64
}

func attachCounting(t *testing.T, book *ledger.Ledger) *countingReplicator {
	t.Helper()
	r := &countingReplicator{book: book}
	if err := book.AttachReplication(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *countingReplicator) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	r.prepared.Add(1)
	version, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: version, CoordinatorEpoch: 1}, err
}

func (r *countingReplicator) Propose(_ context.Context, write ledger.ReplicatedWrite) ([]byte, error) {
	return r.book.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload)
}

package artifact

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
)

// westIssuer issues the leases of region west from a ledger of its own.
// afterCheck, once set, runs once right after a check that passed and
// before the caller goes on: where a lock another region issued can run
// out and change hands.
type westIssuer struct {
	*ledger.Ledger
	mu         sync.Mutex
	afterCheck func(ledger.Lease)
}

func (w *westIssuer) Check(ctx context.Context, lease ledger.Lease) error {
	if err := w.Ledger.Check(ctx, lease); err != nil {
		return err
	}
	w.mu.Lock()
	after := w.afterCheck
	w.afterCheck = nil
	w.mu.Unlock()
	if after != nil {
		after(lease)
	}
	return nil
}

// westNode puts the project's home node in region west.
type westNode struct{ *failingNode }

func (westNode) Region(context.Context, string) (string, error) { return "west", nil }

// foreignLock is commitFixture with the canonical lock issued by region
// west, not by the hub's own ledger, and next the holder west hands the
// lock to.
type foreignLock struct {
	store             *Store
	west              *westIssuer
	node              *failingNode
	canonical, result string
	next              ledger.Lease
}

func newForeignLock(t *testing.T) *foreignLock {
	t.Helper()
	store, _, node, canonical, result := commitFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	book.SetRegion("west")
	f := &foreignLock{store: store, west: &westIssuer{Ledger: book}, node: node, canonical: canonical, result: result}
	store.ledger.SetRegion("east")
	store.ledger.RegisterIssuer("west", f.west)
	store.nodes = westNode{node}
	return f
}

// handOverAfterNextCheck has west, right after the next check passes,
// take that lease away and give the lock to the next holder. The holder
// does what every holder does before it writes — names a snapshot of the
// workspace under its lock — and then writes c. The slot this process
// keeps is left out, as a holder in another process would.
func (f *foreignLock) handOverAfterNextCheck(t *testing.T) {
	f.west.mu.Lock()
	defer f.west.mu.Unlock()
	f.west.afterCheck = func(lease ledger.Lease) {
		ctx := context.Background()
		if err := f.west.Invalidate(ctx, lease.Key); err != nil {
			t.Error(err)
		}
		next, err := f.west.Acquire(ctx, lease.Key, "next", time.Minute)
		if err != nil {
			t.Error(err)
		}
		next.Region = "west"
		f.next = next
		p, _, _ := f.store.projects.Get(ctx, "p")
		parent, _ := f.store.CanonicalOf(ctx, "p")
		if _, _, _, err := f.store.snapshotCanonical(ctx, p, next, parent, "next", "next holder"); err != nil {
			t.Errorf("next holder's snapshot: %v", err)
		}
		write(t, f.canonical, "c", "from next")
	}
}

// keepsNextsWork recovers the landing once the next holder lets go: the
// canonical workspace and name hold the landing and the next holder's c.
func (f *foreignLock) keepsNextsWork(t *testing.T, land Landing) {
	t.Helper()
	f.node.before = nil
	if err := f.west.Release(t.Context(), f.next); err != nil {
		t.Fatal(err)
	}
	recovered, err := f.store.RetryRecoveries(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	changed, err := f.store.Changed(t.Context(), "p", land.Merged, canonicalOf(t, f.store, "p"))
	if err != nil || len(changed) != 1 || changed[0] != "c" {
		t.Fatalf("canonical differs from the merged snapshot in %v (%v), want only the next holder's c", changed, err)
	}
	if read(t, f.canonical, "a") != "a1" || read(t, f.canonical, "c") != "from next" {
		t.Fatalf("a=%q c=%q", read(t, f.canonical, "a"), read(t, f.canonical, "c"))
	}
}

// A commit whose foreign lock ran out just after its check, and went to a
// holder that has named its snapshot and written since, does not move the
// name over that holder's writes: it waits for recovery, which keeps both.
func TestStaleForeignCommitLeavesTheNextHoldersWork(t *testing.T) {
	f := newForeignLock(t)
	p, _, _ := f.store.projects.Get(t.Context(), "p")
	// Once the landing has written, its next check is its commit's.
	f.node.before = onFirst(ops.Apply, func() { f.handOverAfterNextCheck(t) })
	land, err := f.store.Land(t.Context(), p, f.result, "test")
	if !errors.Is(err, ErrRecoveryPending) || f.next.Holder == "" {
		t.Fatalf("land = %+v err=%v, want it waiting for recovery", land, err)
	}
	if head := canonicalOf(t, f.store, "p"); head == land.Merged || head == land.Now {
		t.Fatalf("canonical = %s, want the next holder's snapshot", short(head))
	}
	f.keepsNextsWork(t, land)
}

// A recovery's commit is bounded the same way. The landing's apply fails
// and the node goes away half way through settling it, leaving b for the
// recovery to write.
func TestStaleForeignRecoveryCommitLeavesTheNextHoldersWork(t *testing.T) {
	f := newForeignLock(t)
	p, _, _ := f.store.projects.Get(t.Context(), "p")
	f.node.fail = ops.Apply
	f.node.before = onNth(ops.WritePath, 2, func() { f.node.down = true })
	land, err := f.store.Land(t.Context(), p, f.result, "test")
	if !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	f.node.down, f.node.fail = false, ""
	// Once the recovery has written, its next check is its commit's.
	f.node.before = onFirst(ops.WritePath, func() { f.handOverAfterNextCheck(t) })
	if recovered, err := f.store.RetryRecoveries(t.Context()); err != nil || len(recovered) != 0 || f.next.Holder == "" {
		t.Fatalf("recovered = %+v err=%v, want the commit refused", recovered, err)
	}
	if state := landingState(t, f.store, land.ID); state != LandRecoveryPending {
		t.Fatalf("landing is %s, want it still waiting for recovery", state)
	}
	f.keepsNextsWork(t, land)
}

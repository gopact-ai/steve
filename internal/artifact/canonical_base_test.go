package artifact

import (
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

// An isolated step asking for the canonical state while a landing writes
// the canonical workspace starts from the snapshot the landing merged
// onto: it neither cuts its base from the half-written workspace nor moves
// the canonical name under the landing, which commits.
func TestIsolatedBaseWhileALandingAppliesIsWhatItMergedOnto(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	var ws project.Workspace
	node.before = onFirst(ops.Apply, func() {
		// The apply has written a and not yet b.
		write(t, canonical, "a", "a1")
		var err error
		ws, err = store.Materialize(t.Context(), project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-2"})
		if err != nil {
			t.Error(err)
		}
		write(t, canonical, "a", "a0")
	})

	land, err := store.Land(t.Context(), p, result, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
	if ws.Base != land.Now || read(t, ws.Path, "a") != "a0" || read(t, ws.Path, "b") != "b0" {
		t.Fatalf("isolated base = %s with a=%s b=%s, want %s the landing merged onto", ws.Base, read(t, ws.Path, "a"), read(t, ws.Path, "b"), land.Now)
	}
}

// The same holds while a recovery writes the canonical workspace path by
// path under the lock.
func TestIsolatedBaseWhileARecoveryWritesIsWhatItWritesOnto(t *testing.T) {
	store, p, node, _, result := commitFixture(t)
	node.fail = ops.Apply
	var ws project.Workspace
	node.before = onNth(ops.WritePath, 2, func() {
		var err error
		ws, err = store.Materialize(t.Context(), project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-2"})
		if err != nil {
			t.Error(err)
		}
	})

	land, err := store.Land(t.Context(), p, result, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
	if ws.Base != land.Now || read(t, ws.Path, "a") != "a0" || read(t, ws.Path, "b") != "b0" {
		t.Fatalf("isolated base = %s with a=%s b=%s, want %s", ws.Base, read(t, ws.Path, "a"), read(t, ws.Path, "b"), land.Now)
	}
}

// A workspace an interrupted landing half wrote is waiting for recovery
// and is no base for new work, even with the canonical lock free: the
// step starts from the canonical name, which the snapshot is not moved to.
func TestIsolatedBaseIsNotTakenFromAWorkspaceAwaitingRecovery(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)

	ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-2"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Base != land.Now || read(t, ws.Path, "a") != "a0" {
		t.Fatalf("isolated base = %s with a=%s, want %s", ws.Base, read(t, ws.Path, "a"), land.Now)
	}
	if ref, _, _ := store.Resolve(ctx, CanonicalRef("p")); ref.Artifact != land.Now {
		t.Fatalf("canonical = %s, want it left at %s", ref.Artifact, land.Now)
	}

}

// A canonical name that cannot be read is an error, not an empty parent:
// an isolated base is never cut as a new root, off the canonical lineage.
func TestIsolatedBaseFailsWhenTheCanonicalNameCannotBeRead(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, _ := newStore(t, &localNode{}, project.Home{Path: canonical})
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"}); err != nil {
		t.Fatal(err)
	}
	before := manifestCount(t, store)
	breakIt, _ := unreadableCanonical(t, store)
	breakIt()
	write(t, canonical, "a", "a1")

	if ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-2"}); err == nil {
		t.Fatalf("materialized %+v from an unreadable canonical name", ws)
	}
	if after := manifestCount(t, store); after != before {
		t.Fatalf("recorded %d snapshots with the canonical name unreadable", after-before)
	}
}

func manifestCount(t *testing.T, store *Store) int {
	t.Helper()
	all, err := store.ledger.Bindings(t.Context(), manifestKind)
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// Before anything named a canonical snapshot nothing can be landing, so an
// in-place turn holding the lock before its first snapshot does not keep
// an isolated step from starting: its base is cut as the first snapshot of
// the lineage, and the canonical name is left to the holder of the lock.
func TestIsolatedBaseBeforeAnyCanonicalSnapshotLeavesTheNameToTheLockHolder(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	if _, err := store.acquireCanonical(ctx, p, "att-turn"); err != nil {
		t.Fatal(err)
	}

	ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-2"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Base == "" || read(t, ws.Path, "a") != "a0" {
		t.Fatalf("isolated base = %q with a=%s", ws.Base, read(t, ws.Path, "a"))
	}
	if head := canonicalOf(t, store, "p"); head != "" {
		t.Fatalf("canonical = %s, want it left unnamed for the lock holder", head)
	}
}

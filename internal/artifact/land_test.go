package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestLandingMergesIntoCanonicalUnderTheLockAndReportsConflicts(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})

	// A step works in isolation on a; the user edits b in place meanwhile.
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "a1")
	result, _, _ := store.Publish(ctx, ws, base, "att-1", "step")
	write(t, canonical, "b", "b-user")

	land, err := store.Land(ctx, p, result.ID, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b-user" {
		t.Fatalf("canonical after landing: a=%q b=%q", read(t, canonical, "a"), read(t, canonical, "b"))
	}
	if len(land.Paths) != 1 || land.Paths[0] != "a" {
		t.Fatalf("paths = %v", land.Paths)
	}
	// The canonical name moved to the merged snapshot, which is canonical
	// and durable, so the next landing merges against it.
	ref, _, _ := store.Resolve(ctx, CanonicalRef("p"))
	m, _, _ := store.Manifest(ctx, ref.Artifact)
	if ref.Artifact != land.Merged || !m.Canonical || !m.Durable(p) {
		t.Fatalf("canonical ref = %+v manifest = %+v", ref, m)
	}
	// Every path was journaled started then confirmed.
	outcomes, _ := store.ledger.Journal().Reconcile()
	confirmed := 0
	for _, o := range outcomes {
		if o.Effect.Kind == "land-path" && o.Known() {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("confirmed land-path effects = %d", confirmed)
	}
	// The lock was released.
	if _, err := store.ledger.Acquire(ctx, "canonical:p", "someone", 1); err != nil {
		t.Fatalf("canonical lock still held after landing: %v", err)
	}
	_ = store.ledger.InvalidateAll(ctx)

	// A conflicting result: both sides changed a since base.
	other, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, other.Path, "a", "a-other")
	clash, _, _ := store.Publish(ctx, other, base, "att-2", "clash")
	land, err = store.Land(ctx, p, clash.ID, "test")
	var conflict Conflict
	if !errors.As(err, &conflict) || conflict.State != LandMergeConflicted || land.State != LandMergeConflicted {
		t.Fatalf("conflicting land = %+v err=%v", land, err)
	}
	if read(t, canonical, "a") != "a1" {
		t.Fatal("a conflicted landing wrote to the canonical")
	}
	landings, _ := store.Landings(ctx, "p")
	if len(landings) != 2 {
		t.Fatalf("landings = %d", len(landings))
	}
}

func TestLandingOnANodeHomedProject(t *testing.T) {
	ctx := context.Background()
	node := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(node.root, "proj")
	write(t, canonical, "f", "0")
	store, p := newStore(t, node, project.Home{Node: "node-a", Path: canonical})
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "f", "1")
	write(t, ws.Path, "g", "2")
	result, _, err := store.Publish(ctx, ws, base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	land, err := store.Land(ctx, p, result.ID, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if read(t, canonical, "f") != "1" || read(t, canonical, "g") != "2" {
		t.Fatalf("node canonical after landing: f=%q g=%q", read(t, canonical, "f"), read(t, canonical, "g"))
	}
}

func TestDeferredLandingsRunInOrderAndKeepConflicts(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "a1")
	first, _, _ := store.Publish(ctx, ws, base, "att-1", "one")
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, ws2.Path, "a", "a2")
	second, _, _ := store.Publish(ctx, ws2, base, "att-2", "two")
	_ = store.Defer(ctx, "p", first.ID, "att-1")
	_ = store.Defer(ctx, "p", second.ID, "att-2")
	landed, err := store.LandPending(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(landed) != 2 || landed[0].State != LandCommitted || landed[1].State != LandMergeConflicted {
		t.Fatalf("landings = %+v", landed)
	}
	if read(t, canonical, "a") != "a1" {
		t.Fatal("the first landing did not apply or the second overwrote it")
	}
	raw, _ := store.ledger.Bindings(ctx, pendingKind)
	if len(raw) != 1 {
		t.Fatalf("pending after landing = %d, want the conflicted one kept", len(raw))
	}
	_ = ledger.ErrConflict
}

// A result landing directly — a plan step's own, not one a delegation
// queued — used to be lost outright when it conflicted: the queue only
// held what had been deferred, so nothing retried it and nothing could
// resolve it. Both paths now leave the same record.
func TestADirectLandingThatConflictsIsKeptForResolution(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "mine")
	first, _, _ := store.Publish(ctx, ws, base, "att-1", "one")
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, ws2.Path, "a", "theirs")
	second, _, _ := store.Publish(ctx, ws2, base, "att-2", "two")

	if land, err := store.Land(ctx, p, first.ID, "test"); err != nil || land.State != LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	land, err := store.Land(ctx, p, second.ID, "test")
	var conflict Conflict
	if !errors.As(err, &conflict) || land.State != LandMergeConflicted {
		t.Fatalf("second landing = %+v err=%v", land, err)
	}
	stuck, err := store.Stuck(ctx, "p")
	if err != nil || len(stuck) != 1 || stuck[0].Artifact != second.ID || !stuck[0].Resolvable() {
		t.Fatalf("stuck after a direct conflict = %+v err=%v", stuck, err)
	}
}

// A conflict is only a dead end if the half-merged tree is thrown away.
// Git computes it either way, so the landing keeps it: checking it out
// gives both sides with markers between them, which is what an agent (or
// a person) resolves. And because retrying against an unchanged canonical
// would reach the identical conflict, the queue leaves it alone until the
// canonical name moves — without that the sweeper wrote a fresh failed
// landing every thirty seconds forever.
func TestMergeConflictKeepsAMarkedSnapshotAndStopsRetrying(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := canonicalOf(t, store, "p")
	write(t, ws.Path, "a", "mine")
	first, _, _ := store.Publish(ctx, ws, base, "att-1", "one")
	ws2, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-2"})
	write(t, ws2.Path, "a", "theirs")
	second, _, _ := store.Publish(ctx, ws2, base, "att-2", "two")
	_ = store.Defer(ctx, "p", first.ID, "att-1")
	_ = store.Defer(ctx, "p", second.ID, "att-2")

	landed, err := store.LandPending(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(landed) != 2 || landed[1].State != LandMergeConflicted {
		t.Fatalf("landings = %+v", landed)
	}
	marked := landed[1].Conflict
	if marked == "" {
		t.Fatal("the conflicted landing kept no snapshot to resolve from")
	}
	conflicted, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: marked, Owner: "att-3"})
	if err != nil {
		t.Fatal(err)
	}
	if body := read(t, conflicted.Path, "a"); !strings.Contains(body, "<<<<<<<") || !strings.Contains(body, "mine") || !strings.Contains(body, "theirs") {
		t.Fatalf("the marked snapshot is not the conflict: %q", body)
	}

	stuck, err := store.Stuck(ctx, "p")
	if err != nil || len(stuck) != 1 || stuck[0].Artifact != second.ID || !stuck[0].Resolvable() {
		t.Fatalf("stuck = %+v err=%v", stuck, err)
	}
	if len(stuck[0].Paths) != 1 || stuck[0].Paths[0] != "a" {
		t.Fatalf("stuck paths = %v, want the file both sides changed", stuck[0].Paths)
	}

	before, _ := store.Landings(ctx, "p")
	again, err := store.LandPending(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second pass landed %d, want the conflict left alone", len(again))
	}
	after, _ := store.Landings(ctx, "p")
	if len(after) != len(before) {
		t.Fatalf("landings recorded = %d then %d, want the same conflict not written twice", len(before), len(after))
	}

	// Resolving it is publishing from that workspace, which only works if
	// the marked snapshot is a recorded artifact: a result whose parent is
	// unknown cannot land. Landing the resolution moves canonical, which
	// unblocks the original result — it is now an ancestor of canonical,
	// so it merges to no change and leaves the queue.
	write(t, conflicted.Path, "a", "mine and theirs")
	fixed, _, err := store.Publish(ctx, conflicted, marked, "att-3", "resolve")
	if err != nil {
		t.Fatalf("publish the resolution: %v", err)
	}
	landed2, err := store.Land(ctx, p, fixed.ID, "test")
	if err != nil || landed2.State != LandCommitted {
		t.Fatalf("land the resolution = %+v err=%v", landed2, err)
	}
	if got := read(t, canonical, "a"); got != "mine and theirs" {
		t.Fatalf("canonical after resolving = %q", got)
	}
	final, err := store.LandPending(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 1 || final[0].State != LandCommitted {
		t.Fatalf("the unblocked result = %+v", final)
	}
	rest, err := store.Stuck(ctx, "p")
	if err != nil || len(rest) != 0 {
		t.Fatalf("stuck after resolving = %+v err=%v", rest, err)
	}
	if got := read(t, canonical, "a"); got != "mine and theirs" {
		t.Fatalf("the unblocked result overwrote the resolution: %q", got)
	}
}

package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// crashMidApply builds the state a crash leaves: a landing in applying with
// its WAL started for every path, some paths already written, one not.
func crashMidApply(t *testing.T, store *Store, p project.Project, canonical string) (Landing, string) {
	t.Helper()
	ctx := context.Background()
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	write(t, canonical, "c", "c0")
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := store.canonicalRef(ctx, "p")
	write(t, ws.Path, "a", "a1")
	write(t, ws.Path, "b", "b1")
	write(t, ws.Path, "c", "c1")
	result, _, _ := store.Publish(ctx, ws, base, "att-1", "step")
	now, _, _ := store.SnapshotCanonical(ctx, p, base, "test", "now")
	repo, _ := store.Repo(ctx, "p")
	merged, conflicts, err := repo.Merge(ctx, base, now.ID, result.ID, "land")
	if err != nil || len(conflicts) > 0 {
		t.Fatal(err, conflicts)
	}
	land := Landing{ID: "land-crash", Project: "p", Artifact: result.ID, Base: base, Now: now.ID, Merged: merged, By: "test",
		State: LandApplying, Paths: []string{"a", "b", "c"}, Round: 1}
	if _, err := store.ledger.Begin(ctx, land.ID, landKind, LandApplying, "test", land); err != nil {
		t.Fatal(err)
	}
	for _, path := range land.Paths {
		_, _ = store.ledger.Journal().Started(ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: "1/" + path}, "", nil)
	}
	// The crash happened after a was written, before b and c.
	write(t, canonical, "a", "a1")
	return land, merged
}

func TestRecoveryFinishesALandingCutOffMidApply(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	_, merged := crashMidApply(t, store, p, canonical)

	recovered, err := store.RecoverLandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].State != LandCommitted || recovered[0].Round != 2 {
		t.Fatalf("recovered = %+v", recovered)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" || read(t, canonical, "c") != "c1" {
		t.Fatalf("canonical after recovery: a=%q b=%q c=%q", read(t, canonical, "a"), read(t, canonical, "b"), read(t, canonical, "c"))
	}
	ref, _, _ := store.Resolve(ctx, CanonicalRef("p"))
	if ref.Artifact != merged {
		t.Fatalf("canonical ref = %s, want the merged snapshot %s", ref.Artifact, merged)
	}
	// Round 2 effects were journaled for exactly the rewritten paths; the
	// round-1 starts stay outcome-unknown, as they should: nobody confirmed
	// them.
	outcomes, _ := store.ledger.Journal().Reconcile()
	round2 := 0
	for _, o := range outcomes {
		if o.Effect.Kind == "land-path" && o.Known() && len(o.Effect.InstanceKey) > 2 && o.Effect.InstanceKey[:2] == "2/" {
			round2++
		}
	}
	if round2 != 2 {
		t.Fatalf("round-2 confirmed paths = %d, want b and c", round2)
	}
	unknown := 0
	for _, o := range outcomes {
		if o.Effect.Kind == "land-path" && !o.Known() {
			unknown++
		}
	}
	if unknown != 3 {
		t.Fatalf("round-1 starts still outcome-unknown = %d, want all 3: recovery must not forge confirmations", unknown)
	}
	// The lock was taken under a new epoch and released.
	lease, _, _ := store.ledger.LeaseOf(ctx, "canonical:p")
	if lease.Holder != "" || lease.Epoch < 1 {
		t.Fatalf("lock after recovery = %+v", lease)
	}
	events, _ := store.ledger.Events(ctx, "land-crash")
	last := events[len(events)-1]
	if last.To != LandCommitted || len(last.Fencings) != 1 || last.Fencings[0].Key != "canonical:p" {
		t.Fatalf("commit event = %+v", last)
	}
}

func TestRecoveryStopsAtAPathSomeoneElseChanged(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	_, merged := crashMidApply(t, store, p, canonical)
	// While the hub was down, a person edited c.
	write(t, canonical, "c", "c-by-hand")

	recovered, err := store.RecoverLandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].State != LandApplyConflicted || fmt.Sprint(recovered[0].Paths) != "[c]" {
		t.Fatalf("recovered = %+v", recovered)
	}
	// b was still at the old content and got its merged version; c was
	// left alone; the canonical name did not move.
	if read(t, canonical, "b") != "b1" || read(t, canonical, "c") != "c-by-hand" {
		t.Fatalf("b=%q c=%q", read(t, canonical, "b"), read(t, canonical, "c"))
	}
	ref, _, _ := store.Resolve(ctx, CanonicalRef("p"))
	if ref.Artifact == merged {
		t.Fatal("a conflicted recovery advanced the canonical name")
	}
}

func TestRecoveryClosesLandingsThatNeverWrote(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	land := Landing{ID: "land-early", Project: "p", Artifact: "x", State: LandLocked, By: "test"}
	if _, err := store.ledger.Begin(ctx, land.ID, landKind, LandLocked, "test", land); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverLandings(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandMergeConflicted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	_ = os.Remove
	_ = filepath.Join
}

func TestSealedProjectStaysAtItsHomeAndTheHubKeepsMetadataOnly(t *testing.T) {
	ctx := context.Background()
	// The hub cleared node-a for sealed data: that is what makes it a home
	// a sealed project may have.
	node := &localNode{root: t.TempDir(), state: t.TempDir(), level: "sealed"}
	canonical := filepath.Join(node.root, "vault")
	write(t, canonical, "secret", "s0")
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(ctx, []project.Project{{ID: "p", Level: project.LevelSealed, Home: project.Home{Node: "node-a", Path: canonical}}}); err != nil {
		t.Fatal(err)
	}
	p, _, _ := projects.Get(ctx, "p")
	store := New(filepath.Join(t.TempDir(), "artifacts"), book, projects, node)

	// Anywhere but home is refused, the hub included.
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Node: "", Isolated: true, Owner: "att-hub"}); err == nil {
		t.Fatal("a sealed project was materialised on the hub")
	}
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	base := store.canonicalRef(ctx, "p")
	hub, _ := store.Repo(ctx, "p")
	if hub.Has(ctx, base) {
		t.Fatal("sealed objects reached the hub")
	}
	m, _, _ := store.Manifest(ctx, base)
	if len(m.Receipts) != 1 || m.Receipts[0].Place != "node-a" || !m.Durable(p) {
		t.Fatalf("sealed manifest = %+v", m)
	}
	write(t, ws.Path, "secret", "s1")
	result, changed, err := store.Publish(ctx, ws, base, "att-1", "step")
	if err != nil || !changed || hub.Has(ctx, result.ID) || !result.Durable(p) {
		t.Fatalf("publish = %+v changed=%v err=%v hubHas=%v", result, changed, err, hub.Has(ctx, result.ID))
	}
	land, err := store.Land(ctx, p, result.ID, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if read(t, canonical, "secret") != "s1" {
		t.Fatal("the sealed landing did not reach the home directory")
	}
}

func TestNodeLevelGatesMaterialization(t *testing.T) {
	ctx := context.Background()
	node := &localNode{root: t.TempDir(), state: t.TempDir(), level: "public"}
	canonical := t.TempDir()
	write(t, canonical, "f", "0")
	book, _ := ledger.Open(t.TempDir(), ledger.Options{})
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	_ = projects.Declare(ctx, []project.Project{{ID: "p", Level: project.LevelRestricted, Home: project.Home{Path: canonical}}})
	store := New(filepath.Join(t.TempDir(), "artifacts"), book, projects, node)
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"}); err == nil {
		t.Fatal("restricted data was materialised on a public node")
	}
	// The hub is internal here, so it does not qualify either: a level is
	// a property of the machine, and the hub is a machine.
	if _, err := store.Materialize(ctx, project.Request{Project: "p", Node: "", Isolated: true, Owner: "att-2"}); err == nil {
		t.Fatal("restricted data was materialised on an internal hub")
	}
}

// The expected-old snapshot, the merged tree and the path list are all on
// record before the first path is written: that is what recovery reads.
func TestLandingRecordsExpectedOldAndMergedBeforeApplying(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	base := store.canonicalRef(ctx, "p")
	write(t, ws.Path, "a", "a1")
	result, _, _ := store.Publish(ctx, ws, base, "att-1", "step")
	land, err := store.Land(ctx, p, result.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := store.ledger.Events(ctx, land.ID)
	var sawApplying bool
	for _, e := range events {
		if e.To == LandApplying {
			sawApplying = true
		}
	}
	if !sawApplying {
		t.Fatal("no applying transition")
	}
	// Reading the operation as recovery would: everything needed is there
	// and consistent with the events.
	op, _, _ := store.ledger.Operation(ctx, land.ID)
	var stored Landing
	if err := json.Unmarshal(op.Data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Now == "" || stored.Merged == "" || len(stored.Paths) != 1 || stored.Round != 1 || stored.Base != base {
		t.Fatalf("stored landing = %+v", stored)
	}
	// Round-1 effect ids carry the round, so a later round cannot be
	// mistaken for this one.
	outcomes, _ := store.ledger.Journal().Reconcile()
	found := false
	for _, o := range outcomes {
		if o.Effect.Operation == land.ID && o.Effect.InstanceKey == "1/a" && o.Known() {
			found = true
		}
	}
	if !found {
		t.Fatal("round-1 effect for path a was not journaled started and confirmed")
	}
}

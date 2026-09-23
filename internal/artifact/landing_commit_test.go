package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

// commitFixture is a node-homed project with a result ready to land, whose
// node runs before ahead of every operation.
func commitFixture(t *testing.T) (*Store, project.Project, *failingNode, string, string) {
	t.Helper()
	ctx := t.Context()
	local := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(local.root, "proj")
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	store, p := newStore(t, local, project.Home{Node: "node-a", Path: canonical})
	node := &failingNode{localNode: local}
	store.nodes = node
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	base := store.canonicalRef(ctx, "p")
	write(t, ws.Path, "a", "a1")
	write(t, ws.Path, "b", "b1")
	result, _, err := store.Publish(ctx, ws, base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	return store, p, node, canonical, result.ID
}

// onFirst runs fn ahead of the first operation of kind.
func onFirst(kind ops.Kind, fn func()) func(ops.Request) {
	done := false
	return func(req ops.Request) {
		if req.Op == kind && !done {
			done = true
			fn()
		}
	}
}

// moveCanonicalInPlace is someone snapshotting the canonical workspace
// in place, with an edit of their own, while the landing holds the lock.
func moveCanonicalInPlace(t *testing.T, store *Store, p project.Project, canonical string) func() string {
	var moved string
	return func() string {
		if moved == "" {
			ctx := context.Background()
			write(t, canonical, "user", "edit")
			m, _, err := store.SnapshotCanonical(ctx, p, store.CanonicalOf(ctx, p.ID), "someone", "in place")
			if err != nil {
				t.Error(err)
			}
			moved = m.ID
		}
		return moved
	}
}

// unreadableCanonical breaks reads of the canonical name while leaving its
// version intact; repair restores it.
func unreadableCanonical(t *testing.T, store *Store) (breakIt, repair func()) {
	set := func(at string) {
		if _, err := store.ledger.DB().Exec(`UPDATE names SET updated_at = ? WHERE name = ?`, at, CanonicalRef("p")); err != nil {
			t.Error(err)
		}
	}
	return func() { set("unreadable") }, func() { set(time.Now().UTC().Format(time.RFC3339Nano)) }
}

// The landing merged onto the canonical snapshot it took under the lock.
// A canonical name moved away from it before the commit is a commit
// conflict; the landing must not move the name back over the newer one.
func TestLandingCommitConflictsWhenTheCanonicalNameMovedWhileApplying(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	move := moveCanonicalInPlace(t, store, p, canonical)
	node.before = onFirst(ops.Apply, func() { move() })

	land, err := store.Land(t.Context(), p, result, "test")
	var conflict Conflict
	if !errors.As(err, &conflict) || conflict.State != LandCommitConflict {
		t.Fatalf("land = %+v err=%v, want a commit conflict", land, err)
	}
	if state := landingState(t, store, land.ID); state != LandCommitConflict {
		t.Fatalf("recorded state = %s, want %s", state, LandCommitConflict)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != move() {
		t.Fatalf("canonical = %s, want the snapshot taken meanwhile %s (merged %s)", ref.Artifact, move(), land.Merged)
	}
}

// A landing that wrote every path but could not read the canonical name at
// commit has not conflicted with anyone. It stays applying for recovery,
// which commits it once the name reads again.
func TestLandingThatCannotReadTheCanonicalNameIsLeftForRecovery(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	breakIt, repair := unreadableCanonical(t, store)
	node.before = onFirst(ops.Apply, breakIt)

	land, err := store.Land(t.Context(), p, result, "test")
	var conflict Conflict
	if err == nil || errors.As(err, &conflict) {
		t.Fatalf("land = %+v err=%v, want a plain error", land, err)
	}
	if state := landingState(t, store, land.ID); state != LandApplying {
		t.Fatalf("recorded state = %s, want %s left for recovery", state, LandApplying)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatal("apply did not write the landing")
	}

	repair()
	recovered, err := store.RetryRecoveries(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
}

// Recovery merges nothing: it finishes onto the snapshot it took under its
// own lock. A canonical name moved away from that snapshot before its
// commit is a commit conflict as well.
func TestRecoveryCommitConflictsWhenTheCanonicalNameMovedWhileWriting(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	node.fail = ops.Apply
	move := moveCanonicalInPlace(t, store, p, canonical)
	node.before = onFirst(ops.WritePath, func() { move() })

	land, err := store.Land(t.Context(), p, result, "test")
	var conflict Conflict
	if !errors.As(err, &conflict) || conflict.State != LandCommitConflict {
		t.Fatalf("land = %+v err=%v, want a commit conflict", land, err)
	}
	if state := landingState(t, store, land.ID); state != LandCommitConflict {
		t.Fatalf("recorded state = %s, want %s", state, LandCommitConflict)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != move() {
		t.Fatalf("canonical = %s, want the snapshot taken meanwhile %s (merged %s)", ref.Artifact, move(), land.Merged)
	}
}

// A recovery that wrote every path but could not read the canonical name
// at commit stays recovery-pending; the next retry commits it.
func TestRecoveryThatCannotReadTheCanonicalNameStaysPending(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	node.fail = ops.Apply
	breakIt, repair := unreadableCanonical(t, store)
	node.before = onFirst(ops.WritePath, breakIt)

	land, err := store.Land(t.Context(), p, result, "test")
	if !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land = %+v err=%v, want ErrRecoveryPending", land, err)
	}
	if state := landingState(t, store, land.ID); state != LandRecoveryPending {
		t.Fatalf("recorded state = %s, want %s", state, LandRecoveryPending)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatal("recovery did not write the landing")
	}

	repair()
	recovered, err := store.RetryRecoveries(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
}

package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
	base := canonicalOf(t, store, "p")
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

// onNth runs fn ahead of the nth operation of kind.
func onNth(kind ops.Kind, n int, fn func()) func(ops.Request) {
	seen := 0
	return func(req ops.Request) {
		if req.Op == kind {
			seen++
			if seen == n {
				fn()
			}
		}
	}
}

// displaceCanonical moves the canonical name to another snapshot behind a
// landing's back, which nothing holding the canonical lock does: the
// commit's own check is what is left to catch it.
func displaceCanonical(t *testing.T, store *Store, to string) func() {
	return func() {
		ctx := context.Background()
		ref, _, err := store.Resolve(ctx, CanonicalRef("p"))
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := store.Bind(ctx, CanonicalRef("p"), ref.Version, to); err != nil {
			t.Error(err)
		}
	}
}

// waitsForRecoverySaying checks a landing is recorded recovery-pending
// with a reason that says so much.
func waitsForRecoverySaying(t *testing.T, store *Store, id, reason string) {
	t.Helper()
	op, ok, err := store.ledger.Operation(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("landing %s: found=%v err=%v", id, ok, err)
	}
	var land Landing
	if err := json.Unmarshal(op.Data, &land); err != nil {
		t.Fatal(err)
	}
	if op.State != LandRecoveryPending || !strings.Contains(land.Error, reason) {
		t.Fatalf("recorded %s saying %q, want %s saying %q", op.State, land.Error, LandRecoveryPending, reason)
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
// A canonical name found elsewhere at commit is not a verdict on the
// landing, which wrote every path: it waits for recovery, saying why, the
// name is not moved back over whatever it points at, and recovery commits
// it against a fresh snapshot.
func TestLandingWhoseCanonicalNameMovedIsLeftForRecovery(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	node.before = onFirst(ops.Apply, displaceCanonical(t, store, result))

	land, err := store.Land(t.Context(), p, result, "test")
	var conflict Conflict
	if !errors.Is(err, ErrRecoveryPending) || errors.As(err, &conflict) {
		t.Fatalf("land = %+v err=%v, want ErrRecoveryPending", land, err)
	}
	waitsForRecoverySaying(t, store, land.ID, "not committed")
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != result {
		t.Fatalf("canonical = %s, want it left where it was moved, %s", ref.Artifact, result)
	}

	recovered, err := store.RetryRecoveries(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatal("the landing is not in the canonical workspace")
	}
}

// A landing that wrote every path but could not read the canonical name at
// commit has not conflicted with anyone. It waits for recovery, saying
// why, which commits it once the name reads again.
func TestLandingThatCannotReadTheCanonicalNameIsLeftForRecovery(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	breakIt, repair := unreadableCanonical(t, store)
	node.before = onFirst(ops.Apply, breakIt)

	land, err := store.Land(t.Context(), p, result, "test")
	var conflict Conflict
	if err == nil || errors.As(err, &conflict) {
		t.Fatalf("land = %+v err=%v, want a plain error", land, err)
	}
	waitsForRecoverySaying(t, store, land.ID, "not committed")
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
// commit leaves it recovery-pending, and the next retry commits it.
func TestRecoveryWhoseCanonicalNameMovedStaysPending(t *testing.T) {
	store, p, node, canonical, result := commitFixture(t)
	node.fail = ops.Apply
	node.before = onFirst(ops.WritePath, displaceCanonical(t, store, result))

	land, err := store.Land(t.Context(), p, result, "test")
	if !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land = %+v err=%v, want ErrRecoveryPending", land, err)
	}
	waitsForRecoverySaying(t, store, land.ID, "not committed")

	recovered, err := store.RetryRecoveries(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if ref, _, _ := store.Resolve(t.Context(), CanonicalRef("p")); ref.Artifact != land.Merged {
		t.Fatalf("canonical = %s, want merged %s", ref.Artifact, land.Merged)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatal("the landing is not in the canonical workspace")
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
	waitsForRecoverySaying(t, store, land.ID, "not committed")
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

// A canonical name gone at commit is reported as absent, not as an empty
// snapshot id.
func TestLandingCommitSaysACanonicalNameIsAbsent(t *testing.T) {
	store, p, node, _, result := commitFixture(t)
	node.before = onFirst(ops.Apply, func() {
		if _, err := store.ledger.DB().Exec(`DELETE FROM names WHERE name = ?`, CanonicalRef("p")); err != nil {
			t.Error(err)
		}
	})

	_, err := store.Land(t.Context(), p, result, "test")
	if err == nil || !strings.Contains(err.Error(), "canonical of p is absent") {
		t.Fatalf("err = %v, want the canonical name reported absent", err)
	}
}

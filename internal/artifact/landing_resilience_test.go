package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// failingNode refuses one kind of operation while fail is set, and every
// operation while down is, the way a node that lost its disk or its
// connection mid-landing would. before runs ahead of each operation, as
// someone working in the directory at that moment would.
type failingNode struct {
	*localNode
	fail   ops.Kind
	down   bool
	before func(req ops.Request)
}

func (n *failingNode) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if n.before != nil {
		n.before(req)
	}
	if n.down || n.fail != "" && req.Op == n.fail {
		return ops.Result{}, errors.New("node refused " + string(req.Op))
	}
	return n.localNode.Artifact(ctx, node, req)
}

func landingState(t *testing.T, s *Store, id string) string {
	t.Helper()
	op, ok, err := s.ledger.Operation(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("landing %s: found=%v err=%v", id, ok, err)
	}
	return op.State
}

func landPathEffects(t *testing.T, s *Store, id string) int {
	t.Helper()
	outcomes, err := s.ledger.Journal().Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range outcomes {
		if o.Effect.Operation == id && o.Effect.Kind == "land-path" {
			n++
		}
	}
	return n
}

// A landing whose recovery cannot take the canonical lock at startup is not
// a reason to refuse to start: it is left recovery-pending, untouched, and
// the periodic retry finishes it once the lock is free.
func TestStartupRecoveryThatCannotLockIsLeftForTheRetry(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, merged := crashMidApply(t, store, p, canonical)
	held, err := store.ledger.Acquire(ctx, "canonical:p", "previous-process", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := store.RecoverLandings(ctx)
	if err != nil {
		t.Fatalf("one unrecoverable landing failed startup: %v", err)
	}
	for _, l := range recovered {
		if l.ID == land.ID {
			t.Fatalf("a deferred recovery was reported as recovered: %+v", l)
		}
	}
	if state := landingState(t, store, land.ID); state != LandRecoveryPending {
		t.Fatalf("state = %s, want %s", state, LandRecoveryPending)
	}
	if read(t, canonical, "b") != "b0" || read(t, canonical, "c") != "c0" {
		t.Fatal("recovery wrote without the canonical lock")
	}

	// Still held: the retry leaves it where it is.
	if retried, err := store.RetryRecoveries(ctx); err != nil || len(retried) != 0 {
		t.Fatalf("retry under a held lock = %+v err=%v", retried, err)
	}
	if err := store.ledger.Release(ctx, held); err != nil {
		t.Fatal(err)
	}
	retried, err := store.RetryRecoveries(ctx)
	if err != nil || len(retried) != 1 || retried[0].State != LandCommitted {
		t.Fatalf("retry = %+v err=%v", retried, err)
	}
	if read(t, canonical, "b") != "b1" || read(t, canonical, "c") != "c1" {
		t.Fatalf("after retry b=%q c=%q", read(t, canonical, "b"), read(t, canonical, "c"))
	}
	if ref, _, _ := store.Resolve(ctx, CanonicalRef("p")); ref.Artifact != merged {
		t.Fatalf("canonical = %s, want %s", ref.Artifact, merged)
	}
}

// While a landing waits for recovery the canonical workspace is half
// written; a new landing must not snapshot and merge onto that.
func TestNewLandingWaitsForAnUnrecoveredLanding(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	held, _ := store.ledger.Acquire(ctx, "canonical:p", "previous-process", time.Minute)
	if _, err := store.RecoverLandings(ctx); err != nil {
		t.Fatal(err)
	}
	_ = store.ledger.Release(ctx, held)

	if _, err := store.Land(ctx, p, land.Artifact, "again"); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land over an unrecovered landing = %v, want ErrRecoveryPending", err)
	}
	if read(t, canonical, "b") != "b0" {
		t.Fatal("a new landing wrote over an unrecovered one")
	}
}

// Recovery inspects the objects where the canonical workspace lives. A
// landing cut off before its merged snapshot reached the home node must
// still be recoverable: the snapshot is on the hub, where it was merged.
func TestRecoveryBringsTheMergedSnapshotToTheHomeNode(t *testing.T) {
	ctx := t.Context()
	node := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(node.root, "proj")
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	store, p := newStore(t, node, project.Home{Node: "node-a", Path: canonical})
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
	// The canonical moved on meanwhile, so the merge makes a new commit.
	write(t, canonical, "c", "c-meanwhile")
	now, _, err := store.SnapshotCanonical(ctx, p, base, "test", "now")
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := store.Repo(ctx, "p")
	merged, conflicts, err := hub.Merge(ctx, base, now.ID, result.ID, "land")
	if err != nil || len(conflicts) > 0 {
		t.Fatal(err, conflicts)
	}
	bare := nodeBare(node.state, "p")
	if store.nodeHas(ctx, "node-a", bare, merged) {
		t.Fatal("setup: the merged snapshot is already at the node")
	}
	land := Landing{Target: p.Home, ID: "land-cut", Project: "p", Artifact: result.ID, Base: base, Now: now.ID, Merged: merged, By: "test",
		State: LandApplying, Paths: []string{"a", "b"}, Round: 1}
	if _, err := store.ledger.Begin(ctx, land.ID, landKind, LandApplying, "test", land); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.RecoverLandings(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatalf("a=%q b=%q", read(t, canonical, "a"), read(t, canonical, "b"))
	}
}

// The write-ahead log only starts once everything recovery will read is
// where it will read it: a merged snapshot that cannot reach the home
// node stops the landing before applying, with nothing journaled.
func TestLandingStagesTheMergedSnapshotAtHomeBeforeTheWAL(t *testing.T) {
	ctx := t.Context()
	local := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(local.root, "proj")
	write(t, canonical, "a", "a0")
	store, p := newStore(t, local, project.Home{Node: "node-a", Path: canonical})
	nodes := &failingNode{localNode: local}
	store.nodes = nodes
	ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "a", "a1")
	result, _, err := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	nodes.fail = ops.Unbundle
	land, err := store.Land(ctx, p, result.ID, "test")
	if err == nil {
		t.Fatalf("landed without the merged snapshot at home: %+v", land)
	}
	events, _ := store.ledger.Events(ctx, land.ID)
	for _, e := range events {
		if e.To == LandApplying {
			t.Fatal("the landing entered applying before its merged snapshot was at home")
		}
	}
	if n := landPathEffects(t, store, land.ID); n != 0 {
		t.Fatalf("journaled %d path writes before the merged snapshot was at home", n)
	}
	if read(t, canonical, "a") != "a0" {
		t.Fatal("canonical written")
	}
}

// Recovery that ends in a conflict writes nothing: the paths still at the
// old content stay there rather than being half brought forward.
func TestRecoveryEndingInAConflictWritesNothing(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	write(t, canonical, "c", "c-by-hand")

	recovered, err := store.RecoverLandings(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandApplyConflicted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if read(t, canonical, "b") != "b0" {
		t.Fatalf("b = %q: recovery wrote a path before ending in a conflict", read(t, canonical, "b"))
	}
	outcomes, _ := store.ledger.Journal().Reconcile()
	for _, o := range outcomes {
		if o.Effect.Operation == land.ID && strings.HasPrefix(o.Effect.InstanceKey, "2/") {
			t.Fatalf("round-2 write journaled for a conflicted recovery: %+v", o.Effect)
		}
	}
}

// Someone else holding the canonical lock is contention, not a conflict:
// nothing is recorded as conflicted, and the queued result stays queued.
func TestLockContentionIsNotRecordedAsAConflict(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	write(t, ws.Path, "a", "a1")
	result, _, _ := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if _, err := store.ledger.Acquire(ctx, "canonical:p", "someone", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Land(ctx, p, result.ID, "test"); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("land under contention = %v, want ErrHeld", err)
	}
	if err := store.Defer(ctx, "p", result.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LandPending(ctx, p); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("land pending under contention = %v, want ErrHeld", err)
	}
	landings, err := store.Landings(ctx, "p")
	if err != nil || len(landings) != 0 {
		t.Fatalf("contention left landing records: %+v err=%v", landings, err)
	}
	if stuck, _ := store.Stuck(ctx, "p"); len(stuck) != 0 {
		t.Fatalf("contention blocked the queued result: %+v", stuck)
	}
}

// applyConflicted queues a result that edits a, and lands it while someone
// edits a in the canonical workspace between the snapshot and the write,
// so git refuses the apply and the landing stops apply-conflicted on a.
func applyConflicted(t *testing.T) (*Store, project.Project, *failingNode, string, string) {
	t.Helper()
	ctx := t.Context()
	local := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(local.root, "proj")
	write(t, canonical, "a", "a0")
	write(t, canonical, "other", "o0")
	store, p := newStore(t, local, project.Home{Node: "node-a", Path: canonical})
	nodes := &failingNode{localNode: local}
	store.nodes = nodes
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	write(t, ws.Path, "a", "a1")
	result, _, _ := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if err := store.Defer(ctx, "p", result.ID, "att-1"); err != nil {
		t.Fatal(err)
	}
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "a", "a-by-hand")
			nodes.before = nil
		}
	}
	landed, err := store.LandPending(ctx, p)
	if err != nil || len(landed) != 1 || landed[0].State != LandApplyConflicted || fmt.Sprint(landed[0].Paths) != "[a]" {
		t.Fatalf("first pass = %+v err=%v", landed, err)
	}
	return store, p, nodes, canonical, result.ID
}

// An apply conflict is blocked like a merge conflict: the queue does not
// retry it against the same canonical, nor once the canonical moves for
// reasons that do not touch its paths. An explicit unblock retries it.
func TestApplyConflictIsBlockedUntilItsPathsChange(t *testing.T) {
	ctx := t.Context()
	store, p, _, canonical, artifact := applyConflicted(t)
	stuck, _ := store.Stuck(ctx, "p")
	if len(stuck) != 1 || stuck[0].Artifact != artifact || stuck[0].Reason == "" || stuck[0].State != LandApplyConflicted || stuck[0].Canonical == "" {
		t.Fatalf("stuck = %+v", stuck)
	}
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("an apply conflict was retried against the same canonical: %+v err=%v", again, err)
	}

	write(t, canonical, "other", "moved")
	if _, _, err := store.SnapshotCanonical(ctx, p, canonicalOf(t, store, "p"), "user", "moved"); err != nil {
		t.Fatal(err)
	}
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("an apply conflict was retried because an unrelated path moved: %+v err=%v", again, err)
	}

	// The owner puts a back and asks for the result to land again.
	write(t, canonical, "a", "a0")
	if err := store.Unblock(ctx, "p", artifact, stuck[0].Landing); err != nil {
		t.Fatal(err)
	}
	landed, err := store.LandPending(ctx, p)
	if err != nil || len(landed) != 1 || landed[0].State != LandCommitted || read(t, canonical, "a") != "a1" {
		t.Fatalf("after unblock = %+v err=%v", landed, err)
	}
}

// Once one of the conflicted paths changes in the canonical, the conflict
// may be different, so the queue tries again.
func TestApplyConflictIsRetriedWhenOneOfItsPathsChanges(t *testing.T) {
	ctx := t.Context()
	store, p, _, canonical, _ := applyConflicted(t)
	write(t, canonical, "a", "a-by-hand-again")
	if _, _, err := store.SnapshotCanonical(ctx, p, canonicalOf(t, store, "p"), "user", "moved"); err != nil {
		t.Fatal(err)
	}
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 1 {
		t.Fatalf("a changed conflicted path did not retry: %+v err=%v", again, err)
	}
}

// A recovery that ends in a conflict leaves the workspace partly written by
// the interrupted round. Snapshotting that — the canonical moving because
// of the landing's own writes — must not start the result landing again.
func TestRecoveredConflictIsNotRetriedForItsOwnWrites(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	if err := store.Defer(ctx, "p", land.Artifact, "att-1"); err != nil {
		t.Fatal(err)
	}
	write(t, canonical, "c", "c-by-hand")
	recovered, err := store.RecoverLandings(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandApplyConflicted {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if stuck, _ := store.Stuck(ctx, "p"); len(stuck) != 1 || stuck[0].Artifact != land.Artifact {
		t.Fatalf("stuck = %+v", stuck)
	}
	if _, _, err := store.SnapshotCanonical(ctx, p, canonicalOf(t, store, "p"), "user", "after recovery"); err != nil {
		t.Fatal(err)
	}
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("the recovery's own writes re-armed the retry: %+v err=%v", again, err)
	}
}

// Files under a nested git repository of the canonical workspace are not
// part of its snapshots, so a result that writes there cannot be merged
// with what is really on disk. The landing refuses up front, says why,
// writes and journals nothing, and is not retried in a loop.
func TestLandingRefusesPathsInsideANestedRepositoryUpFront(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, onNode := range []bool{false, true} {
		t.Run(map[bool]string{false: "hub", true: "node"}[onNode], func(t *testing.T) {
			ctx := t.Context()
			local := &localNode{root: t.TempDir(), state: t.TempDir()}
			canonical := filepath.Join(local.root, "proj")
			home := project.Home{Path: canonical}
			if onNode {
				home.Node = "node-a"
			}
			write(t, canonical, "top.md", "top")
			write(t, canonical, "inner/lib.go", "package lib\n")
			if out, err := exec.Command("git", "-C", filepath.Join(canonical, "inner"), "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			store, p := newStore(t, local, home)
			ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
			if err != nil {
				t.Fatal(err)
			}
			// The isolated copy never had inner/, so the child recreates it.
			write(t, ws.Path, "inner/lib.go", "package lib // child\n")
			write(t, ws.Path, "top.md", "top from child")
			result, _, err := store.Publish(ctx, ws, ws.Base, "att-1", "step")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Defer(ctx, "p", result.ID, "att-1"); err != nil {
				t.Fatal(err)
			}
			landed, err := store.LandPending(ctx, p)
			if err != nil || len(landed) != 1 || landed[0].State != LandApplyConflicted {
				t.Fatalf("landing = %+v err=%v", landed, err)
			}
			land := landed[0]
			if !strings.Contains(land.Error, "nested git repository") || !strings.Contains(land.Error, "inner") {
				t.Fatalf("reason = %q, want it to name the nested repository", land.Error)
			}
			if len(land.Paths) != 1 || land.Paths[0] != "inner/lib.go" {
				t.Fatalf("paths = %v, want the paths inside the nested repository", land.Paths)
			}
			if read(t, canonical, "inner/lib.go") != "package lib\n" || read(t, canonical, "top.md") != "top" {
				t.Fatal("a refused landing wrote to the canonical workspace")
			}
			if _, err := os.Stat(filepath.Join(canonical, "inner", ".git")); err != nil {
				t.Fatalf("the nested repository was touched: %v", err)
			}
			if n := landPathEffects(t, store, land.ID); n != 0 {
				t.Fatalf("journaled %d path writes for a refused landing", n)
			}
			stuck, _ := store.Stuck(ctx, "p")
			if len(stuck) != 1 || !strings.Contains(stuck[0].Reason, "nested git repository") {
				t.Fatalf("stuck = %+v", stuck)
			}
			if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
				t.Fatalf("refused landing was retried: %+v err=%v", again, err)
			}
		})
	}
}

// A landing cut off mid-apply by code that did not yet refuse nested
// repositories may have paths inside one. Recovery refuses them just as a
// new landing does, before writing anything: a file the snapshots never
// saw reads as "old" and would otherwise be written into the user's own
// repository.
func TestRecoveryRefusesPathsInsideANestedRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, onNode := range []bool{false, true} {
		t.Run(map[bool]string{false: "hub", true: "node"}[onNode], func(t *testing.T) {
			ctx := t.Context()
			local := &localNode{root: t.TempDir(), state: t.TempDir()}
			canonical := filepath.Join(local.root, "proj")
			home := project.Home{Path: canonical}
			if onNode {
				home.Node = "node-a"
			}
			write(t, canonical, "top.md", "top")
			write(t, canonical, "inner/lib.go", "package lib\n")
			if out, err := exec.Command("git", "-C", filepath.Join(canonical, "inner"), "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			store, p := newStore(t, local, home)
			ws, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
			if err != nil {
				t.Fatal(err)
			}
			base := ws.Base
			write(t, ws.Path, "inner/new.go", "package lib // child\n")
			result, _, err := store.Publish(ctx, ws, base, "att-1", "step")
			if err != nil {
				t.Fatal(err)
			}
			now, _, err := store.SnapshotCanonical(ctx, p, canonicalOf(t, store, "p"), "test", "now")
			if err != nil {
				t.Fatal(err)
			}
			hub, _ := store.Repo(ctx, "p")
			merged, conflicts, err := hub.Merge(ctx, base, now.ID, result.ID, "land")
			if err != nil || len(conflicts) > 0 {
				t.Fatal(err, conflicts)
			}
			if onNode && store.nodeHas(ctx, "node-a", nodeBare(local.state, "p"), merged) {
				t.Fatal("setup: the merged snapshot is already at the node")
			}
			land := Landing{Target: p.Home, ID: "land-nested", Project: "p", Artifact: result.ID, Base: base, Now: now.ID, Merged: merged, By: "att-1",
				State: LandRecoveryPending, Paths: []string{"inner/new.go"}, Round: 1}
			if _, err := store.ledger.Begin(ctx, land.ID, landKind, LandRecoveryPending, "test", land); err != nil {
				t.Fatal(err)
			}
			if err := store.Defer(ctx, "p", result.ID, "att-1"); err != nil {
				t.Fatal(err)
			}

			recovered, err := store.RecoverLandings(ctx)
			if err != nil || len(recovered) != 1 || recovered[0].State != LandApplyConflicted {
				t.Fatalf("recovered = %+v err=%v", recovered, err)
			}
			if _, err := os.Stat(filepath.Join(canonical, "inner", "new.go")); !os.IsNotExist(err) {
				t.Fatalf("recovery wrote into the nested repository: %v", err)
			}
			// The merged snapshot was only on the hub; recovery staged it at
			// home, where it reads, before deciding anything.
			if onNode && !store.nodeHas(ctx, "node-a", nodeBare(local.state, "p"), merged) {
				t.Fatal("recovery did not bring the merged snapshot to the home node")
			}
			if !strings.Contains(recovered[0].Error, "nested git repository") || fmt.Sprint(recovered[0].Paths) != "[inner/new.go]" {
				t.Fatalf("recovered = error %q paths %v", recovered[0].Error, recovered[0].Paths)
			}
			if n := landPathEffects(t, store, land.ID); n != 0 {
				t.Fatalf("journaled %d path writes for a refused recovery", n)
			}
			stuck, _ := store.Stuck(ctx, "p")
			if len(stuck) != 1 || stuck[0].Artifact != result.ID || !strings.Contains(stuck[0].Reason, "nested git repository") || stuck[0].State != LandApplyConflicted {
				t.Fatalf("stuck = %+v", stuck)
			}
			if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
				t.Fatalf("refused recovery was retried: %+v err=%v", again, err)
			}
		})
	}
}

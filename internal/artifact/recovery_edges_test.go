package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// nodeLanding sets up a project whose canonical workspace is on a node and
// a published result that changes a and b there.
func nodeLanding(t *testing.T) (*Store, project.Project, *failingNode, string, string) {
	t.Helper()
	ctx := t.Context()
	local := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(local.root, "proj")
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	store, p := newStore(t, local, project.Home{Node: "node-a", Path: canonical})
	nodes := &failingNode{localNode: local}
	store.nodes = nodes
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	write(t, ws.Path, "a", "a1")
	write(t, ws.Path, "b", "b1")
	result, _, err := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	return store, p, nodes, canonical, result.ID
}

// An apply that fails part-way has written some paths and not others. It
// is not a conflict: the landing is finished path by path, at once.
func TestApplyThatFailsPartWayIsFinished(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, artifact := nodeLanding(t)
	// The node writes a, then refuses the rest of the apply.
	nodes.fail = ops.Apply
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "a", "a1")
			nodes.before = nil
		}
	}
	land, err := store.Land(ctx, p, artifact, "test")
	if err != nil || land.State != LandCommitted {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatalf("a=%q b=%q", read(t, canonical, "a"), read(t, canonical, "b"))
	}
	if stuck, _ := store.Stuck(ctx, "p"); len(stuck) != 0 {
		t.Fatalf("a finished landing left the result stuck: %+v", stuck)
	}
}

// When the workspace cannot even be inspected after a failed apply — the
// node went away — what it holds is unknown. The landing waits for
// recovery, and until then no new landing merges onto that workspace.
func TestApplyFailureThatCannotBeInspectedWaitsForRecovery(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, artifact := nodeLanding(t)
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "a", "a1")
			nodes.down, nodes.before = true, nil
		}
	}
	land, err := store.Land(ctx, p, artifact, "test")
	if !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land = %+v err=%v, want ErrRecoveryPending", land, err)
	}
	if state := landingState(t, store, land.ID); state != LandRecoveryPending {
		t.Fatalf("state = %s, want %s", state, LandRecoveryPending)
	}
	nodes.down = false
	if _, err := store.Land(ctx, p, artifact, "again"); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("a new landing over the unrecovered one = %v, want ErrRecoveryPending", err)
	}
	recovered, err := store.RetryRecoveries(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("retry = %+v err=%v", recovered, err)
	}
	if read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b1" {
		t.Fatalf("a=%q b=%q", read(t, canonical, "a"), read(t, canonical, "b"))
	}
}

// A delegated result lands under its parent's lock. If the process dies
// while it applies, that lock is still the parent's — the parent is
// resumed after a restart with it — and recovery must not release it.
func TestStartupRecoveryKeepsALentLock(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	parent, err := store.ledger.Acquire(ctx, "canonical:p", "parent-turn", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The record the landing left: applying, under the lock it was lent.
	land, _ := crashMidApplyAs(t, store, p, canonical, func(land *Landing) {
		land.Lease, land.Borrowed = &parent, true
	})

	if _, err := store.RecoverLandings(ctx); err != nil {
		t.Fatal(err)
	}
	if held, _, _ := store.ledger.LeaseOf(ctx, "canonical:p"); held.Holder != "parent-turn" || held.Epoch != parent.Epoch {
		t.Fatalf("lock after recovery = %+v, want the parent's %+v", held, parent)
	}
	if state := landingState(t, store, land.ID); state != LandRecoveryPending {
		t.Fatalf("state = %s, want it waiting for the parent", state)
	}
	if read(t, canonical, "b") != "b0" {
		t.Fatal("recovery wrote under a lock that is not its own")
	}
}

// A landing cut off in a directory the project has since moved away from
// can never be finished there or in the new one. Recovery ends it, says
// why, and the project can take landings again.
func TestRecoveryEndsALandingWhoseProjectMoved(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	if err := store.Defer(ctx, "p", land.Artifact, "att-1"); err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	write(t, moved, "a", "a0")
	if err := store.projects.Declare(ctx, []project.Project{{ID: "p", Home: project.Home{Path: moved}}}); err != nil {
		t.Fatal(err)
	}

	recovered, err := store.RecoverLandings(ctx)
	if err != nil || len(recovered) != 1 || recovered[0].State != LandApplyConflicted || !strings.Contains(recovered[0].Error, "moved") {
		t.Fatalf("recovered = %+v err=%v", recovered, err)
	}
	if read(t, canonical, "b") != "b0" || read(t, moved, "a") != "a0" {
		t.Fatal("recovery wrote into either directory")
	}
	stuck, _ := store.Stuck(ctx, "p")
	if len(stuck) != 1 || !strings.Contains(stuck[0].Reason, "moved") {
		t.Fatalf("stuck = %+v, want the result kept with the reason", stuck)
	}
	if err := store.checkNoRecoveryPending(ctx, p, ""); err != nil {
		t.Fatalf("the project still refuses landings: %v", err)
	}
}

// Asking to land a stuck result again names the stop the owner looked at.
// A result that has since stopped somewhere else, or one stopped on a
// merge that is resolved rather than retried, is not unblocked blindly.
func TestUnblockOnlyClearsTheStopTheCallerSaw(t *testing.T) {
	ctx := t.Context()
	store, _, _, _, artifact := applyConflicted(t)
	stuck, _ := store.Stuck(ctx, "p")
	if len(stuck) != 1 {
		t.Fatalf("stuck = %+v", stuck)
	}
	if err := store.Unblock(ctx, "p", artifact, "land-seen-earlier"); !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("unblock of another stop = %v, want ErrNotBlocked", err)
	}
	if again, _ := store.Stuck(ctx, "p"); len(again) != 1 {
		t.Fatal("a stale unblock cleared the current stop")
	}
	if err := store.Unblock(ctx, "p", artifact, stuck[0].Landing); err != nil {
		t.Fatal(err)
	}
	if err := store.Unblock(ctx, "p", artifact, stuck[0].Landing); !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("second unblock = %v, want ErrNotBlocked", err)
	}
}

// A delegated result that stopped on a conflict while landing under its
// parent is queued by the delegation afterwards. Queuing it again must not
// forget why it is stuck, or the next pass lands it into the same wall.
func TestDeferringAStuckResultKeepsItBlocked(t *testing.T) {
	ctx := t.Context()
	store, p, _, _, artifact := applyConflicted(t)
	before, _ := store.Stuck(ctx, "p")
	if err := store.Defer(ctx, "p", artifact, "task #2"); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Stuck(ctx, "p")
	if len(after) != 1 || len(before) != 1 || after[0].Landing != before[0].Landing || !after[0].At.Equal(before[0].At) {
		t.Fatalf("stuck before %+v, after deferring again %+v", before, after)
	}
	if again, err := store.LandPending(ctx, p); err != nil || len(again) != 0 {
		t.Fatalf("the re-queued result was retried against the same canonical: %+v err=%v", again, err)
	}
}

// A transition that is refused leaves the landing as it was: what the
// caller holds must say what the record says.
func TestRefusedTransitionKeepsTheLandingState(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	land := Landing{ID: "land-x", Project: "p", State: LandLocked}
	if _, err := store.ledger.Begin(ctx, land.ID, landKind, LandLocked, "test", land); err != nil {
		t.Fatal(err)
	}
	if err := store.move(ctx, &land, LandMerged, LandApplying, nil); err == nil {
		t.Fatal("a transition from the wrong state was accepted")
	}
	if land.State != LandLocked {
		t.Fatalf("state = %s after a refused transition, want %s", land.State, LandLocked)
	}
}

// macOS file systems ignore case by default: a result that writes Inner/x
// writes into inner/, a nested repository, all the same.
func TestPathsInsideNestedIgnoreCase(t *testing.T) {
	inside, repos := pathsInsideNested([]string{"Inner/x.go", "inner", "innerx/y", "top.md"}, []string{"inner"})
	if fmt.Sprint(inside) != "[Inner/x.go inner]" || fmt.Sprint(repos) != "[inner]" {
		t.Fatalf("inside = %v repos = %v", inside, repos)
	}
}

// A sweep that comes by while a failed apply is being recovered in place
// finds the lock held and leaves the landing alone: two recoveries of one
// landing never run at once, and the lock is not released under it.
func TestRetryLeavesARecoveryInProgressAlone(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, artifact := nodeLanding(t)
	nodes.fail = ops.Apply
	swept := false
	nodes.before = func(req ops.Request) {
		switch {
		case req.Op == ops.Apply:
			write(t, canonical, "a", "a1")
		case req.Op == ops.PathState && !swept:
			swept = true
			held, _, _ := store.ledger.LeaseOf(ctx, "canonical:p")
			recovered, err := store.RetryRecoveries(ctx)
			if err != nil || len(recovered) != 0 {
				t.Errorf("a sweep during the recovery recovered %+v err=%v", recovered, err)
			}
			if after, _, _ := store.ledger.LeaseOf(ctx, "canonical:p"); after != held {
				t.Errorf("lock during the recovery went from %+v to %+v", held, after)
			}
		}
	}
	land, err := store.Land(ctx, p, artifact, "test")
	if !swept || err != nil || land.State != LandCommitted {
		t.Fatalf("swept=%v land=%+v err=%v", swept, land, err)
	}
	if read(t, canonical, "b") != "b1" {
		t.Fatalf("b=%q", read(t, canonical, "b"))
	}
}

// A result that turns a file into a directory cannot be finished path by
// path. When its apply fails, the landing ends apply-conflicted with the
// reason, instead of waiting for a recovery that can never succeed while
// the project refuses every other landing.
func TestFailedApplyThatTurnsAFileIntoADirectoryEnds(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, result := fileBecomesDirectory(t)
	// Someone edits x as the apply starts, and git refuses it.
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "x", "x-by-hand")
			nodes.before = nil
		}
	}
	land, err := store.Land(ctx, p, result, "test")
	var conflict Conflict
	if !errors.As(err, &conflict) || land.State != LandApplyConflicted || !strings.Contains(land.Error, "between files and directories") {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	if err := store.checkNoRecoveryPending(ctx, p, ""); err != nil {
		t.Fatalf("the project still refuses landings: %v", err)
	}
	if read(t, canonical, "a") != "a0" {
		t.Fatalf("a=%q", read(t, canonical, "a"))
	}
}

// An apply that turned a file into a directory completely before it
// reported failing has nothing left to write: it is finished, not called
// a conflict.
func TestFailedApplyThatFinishedTurningAFileIntoADirectoryIsCommitted(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, result := fileBecomesDirectory(t)
	nodes.fail = ops.Apply
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			nodes.before = nil
			if _, err := nodes.localNode.Artifact(ctx, "node-a", req); err != nil {
				t.Errorf("apply: %v", err)
			}
		}
	}
	land, err := store.Land(ctx, p, result, "test")
	if err != nil || land.State != LandCommitted || read(t, canonical, "a/b") != "b1" {
		t.Fatalf("land = %+v err=%v a/b=%q", land, err, read(t, canonical, "a/b"))
	}
}

// fileBecomesDirectory publishes a result that replaces the file a with a
// directory holding a/b, next to an untouched x.
func fileBecomesDirectory(t *testing.T) (*Store, project.Project, *failingNode, string, string) {
	t.Helper()
	ctx := t.Context()
	local := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(local.root, "proj")
	write(t, canonical, "a", "a0")
	write(t, canonical, "x", "x0")
	store, p := newStore(t, local, project.Home{Node: "node-a", Path: canonical})
	nodes := &failingNode{localNode: local}
	store.nodes = nodes
	ws, _ := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	if err := os.Remove(filepath.Join(ws.Path, "a")); err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "a/b", "b1")
	result, _, err := store.Publish(ctx, ws, ws.Base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	return store, p, nodes, canonical, result.ID
}

// A durable landing whose apply fails is recovered in place like any
// other: it does not stay applying, where nothing would keep the next
// landing off the half-written workspace.
func TestDurableApplyFailureIsRecovered(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, artifact := nodeLanding(t)
	nodes.fail = ops.Apply
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "a", "a1")
			nodes.down, nodes.before = true, nil
		}
	}
	if _, err := store.LandOnce(ctx, "durable-1", p, artifact, "plan"); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land once = %v, want ErrRecoveryPending", err)
	}
	if state := landingState(t, store, "durable-1"); state != LandRecoveryPending {
		t.Fatalf("state = %s, want %s", state, LandRecoveryPending)
	}
	if err := store.checkNoRecoveryPending(ctx, p, ""); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("a new landing over the half-written workspace = %v, want ErrRecoveryPending", err)
	}
	nodes.down, nodes.fail = false, ""
	land, err := store.LandOnce(ctx, "durable-1", p, artifact, "plan")
	if err != nil || land.State != LandCommitted || read(t, canonical, "b") != "b1" {
		t.Fatalf("resumed land = %+v err=%v b=%q", land, err, read(t, canonical, "b"))
	}
}

// A merge conflict is resolved, not retried, even one whose marked tree
// was never recorded: the result is blocked until the canonical changes.
func TestUnblockRefusesAMergeConflictWithoutATree(t *testing.T) {
	ctx := t.Context()
	store, _ := newStore(t, &localNode{}, project.Home{Path: t.TempDir()})
	item := Pending{Project: "p", Artifact: "art", By: "test", At: time.Now().UTC(),
		Blocked: &Blocked{Landing: "land-m", State: LandMergeConflicted, Canonical: "c0", At: time.Now().UTC()}}
	if err := store.ledger.Update(ctx, func(tx *ledger.Tx) error { return tx.PutBinding(pendingKind, "p/art", item) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Unblock(ctx, "p", "art", "land-m"); !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("unblock of a merge conflict = %v, want ErrNotBlocked", err)
	}
}

package delegate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// busyText is what a parent is told of a result queued behind another
// turn working in the main directory.
const busyText = "排队中：主目录正被别的回合占用，空出来就落地"

// A parent cannot settle a conflict or land a result itself. What follows
// a conflict's paths, and its reason for an apply conflict, is what the
// user can do about it. A merge conflict git kept no marked tree for can
// be neither resolved nor edited, only dealt with on the machine.
const (
	mergeHint  = "。你不能自己解决；用户可以发 /resolve，或在控制台「待处理」的「合并冲突」里处理"
	noTreeHint = "。你不能自己解决；这次冲突没有留下可编辑的快照，用户只能到主目录所在的机器上直接处理，控制台「待处理」的「合并冲突」里列出了它和那台机器"
	applyHint  = "。你不能自己重新落地；用户处理好原因后，可以在控制台「待处理」的「合并冲突」里点「重新落地」"
)

// unfinishedText is what a parent is told of a result whose landing
// reached a conflict the queue does not keep, which is all the console
// and /resolve act on: the landing recorded it but the queue did not, or
// the landing could not record it either and is left locked, or closed
// with nothing of the conflict. Whether the conflict is recorded later,
// and who settles it then, is not known, so it says only what is certain
// and where a recorded conflict is listed.
const unfinishedText = "未落地：这次落地遇到冲突，没有完成；主目录没有改动，结果仍在落地队列里。你不能自己处理；冲突记下后，会和其他落地冲突一样列在控制台「待处理」的「合并冲突」里"

// unfinishedWritingText is unfinishedText for an apply conflict recovery
// reached once the landing had begun writing the main directory, which the
// paths written by then are left in.
const unfinishedWritingText = "未落地：这次落地遇到冲突，没有完成；主目录里可能已经写入了一部分，结果仍在落地队列里。你不能自己处理；冲突记下后，会和其他落地冲突一样列在控制台「待处理」的「合并冲突」里"

// conflictingResults publishes two results that change notes.md from the
// same canonical base in different ways: whichever lands second conflicts.
func conflictingResults(t *testing.T, w *world) (project.Project, artifact.Manifest, artifact.Manifest) {
	t.Helper()
	ctx := t.Context()
	if err := os.WriteFile(filepath.Join(w.home, "notes.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, found, err := w.artifacts.Project(ctx, "p")
	if err != nil || !found {
		t.Fatalf("project p: found=%v err=%v", found, err)
	}
	publish := func(owner, body, base string) (artifact.Manifest, string) {
		ws, err := w.artifacts.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		if base == "" {
			if base, err = w.artifacts.CanonicalOf(ctx, "p"); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(ws.Path, "notes.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		m, _, err := w.artifacts.Publish(ctx, ws, base, owner, body)
		if err != nil {
			t.Fatal(err)
		}
		return m, base
	}
	first, base := publish("att-1", "mine\n", "")
	second, _ := publish("att-2", "theirs\n", base)
	return p, first, second
}

func childWith(artifactID string) task.Task {
	return task.Task{ID: "c", State: task.StateDone, Result: &task.Result{Answer: "done it", Refs: []string{"artifact " + artifactID}}}
}

// A result held on the queue by a landing conflict is not landed, and a
// later pass produces no landing for it. Its parent must be told it is
// stuck, not that it landed earlier or had nothing to land.
func TestLandForSaysAResultStuckOnAConflictIsStuck(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	p, first, second := conflictingResults(t, w)
	if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	var conflict artifact.Conflict
	if _, err := w.artifacts.Land(t.Context(), p, second.ID, "test"); !errors.As(err, &conflict) {
		t.Fatalf("second landing did not conflict: %v", err)
	}

	if got := w.service.landFor(t.Context(), parent, nil)(childWith(second.ID)); got != "落地冲突：notes.md"+mergeHint {
		t.Fatalf("landing text for a stuck result = %q; want the conflict and its path", got)
	}
}

// A conflict reached in this very pass reads as one reached earlier does:
// by what it is, not by the landing's internal state name or cause.
func TestLandForSaysAConflictReachedNowIsAConflict(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	_, first, second := conflictingResults(t, w)
	for _, m := range []artifact.Manifest{first, second} {
		if err := w.artifacts.Defer(t.Context(), "p", m.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}

	landing := w.service.landFor(t.Context(), parent, nil)
	if got := landing(childWith(first.ID)); !strings.HasPrefix(got, "已落地主目录") {
		t.Fatalf("landing text for the landed result = %q", got)
	}
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md"+mergeHint {
		t.Fatalf("landing text for a result that conflicted now = %q; want the conflict and its path", got)
	}
}

// A conflict this pass reached is a conflict even when a result landed
// after it in the same pass has moved the canonical on, so that the next
// pass retries it.
func TestLandForSaysAConflictReachedNowIsAConflictOnceTheCanonicalMoves(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	p, first, second := conflictingResults(t, w)
	if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	later := publishNotes(t, w, "att-3", "later\n")
	for _, m := range []artifact.Manifest{second, later} {
		if err := w.artifacts.Defer(t.Context(), "p", m.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}

	landing := w.service.landFor(t.Context(), parent, nil)
	if got := landing(childWith(later.ID)); !strings.HasPrefix(got, "已落地主目录") {
		t.Fatalf("landing text for the result landed after the conflict = %q", got)
	}
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md"+mergeHint {
		t.Fatalf("landing text for a result that conflicted before the canonical moved = %q; want the conflict and its path", got)
	}
}

// A result a pass skips as stuck on its conflict is stuck, even when a
// result landed later in the same pass moves the canonical on so that the
// next pass retries it: this pass did not.
func TestLandForSaysAResultThePassSkippedAsStuckIsStuck(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	p, first, second := conflictingResults(t, w)
	if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	var conflict artifact.Conflict
	if _, err := w.artifacts.Land(t.Context(), p, second.ID, "test"); !errors.As(err, &conflict) {
		t.Fatalf("second landing did not conflict: %v", err)
	}
	other := publishFile(t, w, "att-3", "other.md", "other\n")
	if err := w.artifacts.Defer(t.Context(), "p", other.ID, "test"); err != nil {
		t.Fatal(err)
	}

	landing := w.service.landFor(t.Context(), parent, nil)
	if got := landing(childWith(other.ID)); !strings.HasPrefix(got, "已落地主目录") {
		t.Fatalf("landing text for the result landed in the pass = %q", got)
	}
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md"+mergeHint {
		t.Fatalf("landing text for a result the pass skipped as stuck = %q; want the conflict and its path", got)
	}
}

// busyMainDirectory has another turn work in the project's main
// directory, which holds its lock until the test ends.
func busyMainDirectory(t *testing.T, w *world) {
	t.Helper()
	if _, err := w.attempts.Open(t.Context(), attempt.Spec{TaskID: "other-turn", Kind: attempt.KindChat, Project: "p", Agent: "codex", Harness: "mock",
		Workspace: project.Workspace{ID: "canonical:p", Project: "p", Path: w.home, Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted}); err != nil {
		t.Fatal(err)
	}
}

// publishNotes publishes a result that writes body to notes.md over the
// canonical as it is now.
func publishNotes(t *testing.T, w *world, owner, body string) artifact.Manifest {
	t.Helper()
	return publishFile(t, w, owner, "notes.md", body)
}

// publishFile publishes a result that writes body to name over the
// canonical as it is now.
func publishFile(t *testing.T, w *world, owner, name, body string) artifact.Manifest {
	t.Helper()
	ctx := t.Context()
	ws, err := w.artifacts.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.artifacts.CanonicalOf(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _, err := w.artifacts.Publish(ctx, ws, base, owner, body)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A result whose conflict the canonical has since moved past is retried by
// the next pass. While another turn keeps that pass out of the main
// directory, it is queued, not stuck: its conflict record is out of date.
func TestLandForSaysAResultWhoseConflictClearedIsQueued(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	p, first, second := conflictingResults(t, w)
	if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	var conflict artifact.Conflict
	if _, err := w.artifacts.Land(t.Context(), p, second.ID, "test"); !errors.As(err, &conflict) {
		t.Fatalf("second landing did not conflict: %v", err)
	}
	resolved := publishNotes(t, w, "att-3", "theirs\n")
	if land, err := w.artifacts.Land(t.Context(), p, resolved.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("resolving landing = %+v err=%v", land, err)
	}
	busyMainDirectory(t, w)

	if got := w.service.landFor(t.Context(), parent, nil)(childWith(second.ID)); got != busyText {
		t.Fatalf("landing text for a result whose conflict cleared = %q; want %q", got, busyText)
	}
}

// A result still stuck on its conflict is stuck whether or not the main
// directory is free; one queued behind the busy directory is queued.
func TestLandForSaysAStuckResultIsStuckWhileTheMainDirectoryIsBusy(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	p, first, second := conflictingResults(t, w)
	if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("first landing = %+v err=%v", land, err)
	}
	var conflict artifact.Conflict
	if _, err := w.artifacts.Land(t.Context(), p, second.ID, "test"); !errors.As(err, &conflict) {
		t.Fatalf("second landing did not conflict: %v", err)
	}
	queued := publishNotes(t, w, "att-3", "later\n")
	if err := w.artifacts.Defer(t.Context(), "p", queued.ID, "test"); err != nil {
		t.Fatal(err)
	}
	busyMainDirectory(t, w)

	landing := w.service.landFor(t.Context(), parent, nil)
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md"+mergeHint {
		t.Fatalf("landing text for a stuck result while the main directory is busy = %q; want the conflict and its path", got)
	}
	if got := landing(childWith(queued.ID)); got != busyText {
		t.Fatalf("landing text for a queued result while the main directory is busy = %q; want %q", got, busyText)
	}
}

// nestedRepoResult publishes a result that writes inner/lib.go, which the
// main directory keeps in a nested git repository: landing it stops at an
// apply conflict.
func nestedRepoResult(t *testing.T, w *world) artifact.Manifest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := t.Context()
	inner := filepath.Join(w.home, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", inner, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	ws, err := w.artifacts.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.artifacts.CanonicalOf(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	// Snapshots leave the nested repository out, so the child writes a
	// file the main directory already has in one.
	if err := os.MkdirAll(filepath.Join(ws.Path, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "inner", "lib.go"), []byte("package lib // child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _, err := w.artifacts.Publish(ctx, ws, base, "att-1", "step")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// queueAll queues results for project p's next landing pass, in order.
func queueAll(t *testing.T, w *world, results ...artifact.Manifest) {
	t.Helper()
	for _, m := range results {
		if err := w.artifacts.Defer(t.Context(), "p", m.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

// unmarkedConflict is conflictingResults on a ledger of the test's own
// that refuses to record the marked tree of a merge conflict, so the
// conflict is left with none to work from.
func unmarkedConflict(t *testing.T, w *world) (project.Project, artifact.Manifest, artifact.Manifest) {
	t.Helper()
	ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
	p, first, second := conflictingResults(t, w)
	if _, err := w.book.DB().Exec(`CREATE TRIGGER refuse_marked BEFORE INSERT ON bindings
		WHEN NEW.kind = 'artifact' AND json_extract(NEW.data, '$.message') LIKE 'conflict landing %'
		BEGIN SELECT RAISE(ABORT, 'marked tree not recorded'); END`); err != nil {
		t.Fatal(err)
	}
	return p, first, second
}

// refuseQueuedConflicts has the ledger of the test's own refuse to keep
// any conflict on the landing queue, as a failed write would, while the
// landings record theirs as usual.
func refuseQueuedConflicts(t *testing.T, w *world) {
	t.Helper()
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := w.book.DB().Exec(`CREATE TRIGGER refuse_queued_conflict_` + event + ` BEFORE ` + event + ` ON bindings
			WHEN NEW.kind = 'pending-landing' AND json_extract(NEW.data, '$.blocked') IS NOT NULL
			BEGIN SELECT RAISE(ABORT, 'conflict not queued'); END`); err != nil {
			t.Fatal(err)
		}
	}
}

// unqueuedConflict checks that the queue keeps no conflict of project p
// and returns the one landing that did not commit, which recorded a
// conflict in state.
func unqueuedConflict(t *testing.T, w *world, state string) artifact.Landing {
	t.Helper()
	stuck, err := w.artifacts.Stuck(t.Context(), "p")
	if err != nil || len(stuck) != 0 {
		t.Fatalf("stuck results = %+v err=%v; want none", stuck, err)
	}
	landings, err := w.artifacts.Landings(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	var conflicted []artifact.Landing
	for _, l := range landings {
		if l.State != artifact.LandCommitted {
			conflicted = append(conflicted, l)
		}
	}
	if len(conflicted) != 1 || conflicted[0].State != state {
		t.Fatalf("landings not committed = %+v; want one, %s", conflicted, state)
	}
	return conflicted[0]
}

// An apply conflict says why it stopped. Reached in this pass, it reads as
// the queue keeps it for every later pass.
func TestLandForSaysAnApplyConflictAsLaterPassesDo(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	ctx := t.Context()
	m := nestedRepoResult(t, w)
	queueAll(t, w, m)

	now := w.service.landFor(ctx, parent, nil)(childWith(m.ID))
	if !strings.HasPrefix(now, "落地冲突：") || !strings.Contains(now, "nested git repository (inner)") || !strings.HasSuffix(now, ": inner/lib.go"+applyHint) {
		t.Fatalf("landing text for an apply conflict reached now = %q; want the conflict, why and its path", now)
	}
	if later := w.service.landFor(ctx, parent, nil)(childWith(m.ID)); later != now {
		t.Fatalf("landing text for the same apply conflict later = %q; reached now it was %q", later, now)
	}
}

// hookedNodes runs before ahead of each artifact operation on a node.
type hookedNodes struct {
	artifact.LocalNodes
	before func(ops.Request)
}

func (n hookedNodes) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	n.before(req)
	return n.LocalNodes.Artifact(ctx, node, req)
}

// homeOnNode moves project p's main directory onto node-a, whose artifact
// operations run here after before. A main directory on the hub does not
// go through nodes, so this is how a test acts in the middle of a landing.
func homeOnNode(t *testing.T, w *world, before func(ops.Request)) {
	t.Helper()
	ownStores(t, w, "node-a", hookedNodes{LocalNodes: artifact.LocalNodes{Dir: t.TempDir()}, before: before})
}

// ownStores gives the world's landings a ledger of the test's own, w.book,
// with project p's main directory on node ("" is here) and its artifact
// operations run by nodes.
func ownStores(t *testing.T, w *world, node string, nodes artifact.Nodes) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Node: node, Path: home}}}); err != nil {
		t.Fatal(err)
	}
	w.book, w.home = book, home
	w.artifacts, w.attempts = artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, nodes), attempt.New(book)
	w.service.SetLedger(w.attempts, w.artifacts)
}

// editBy writes body to name in the main directory, as someone working
// there by hand would.
func editBy(t *testing.T, w *world, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.home, name), []byte(body), 0o644); err != nil {
		t.Error(err)
	}
}

// An apply conflict recovery reached in this pass — a.txt edited by hand
// as the landing starts writing it — reads as the queue keeps it for every
// later pass, without recovery's own cause.
func TestLandForSaysARecoveredApplyConflictAsLaterPassesDo(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	var once sync.Once
	homeOnNode(t, w, func(req ops.Request) {
		if req.Op == ops.Apply {
			once.Do(func() { editBy(t, w, "a.txt", "by hand\n") })
		}
	})
	editBy(t, w, "a.txt", "base\n")
	m := publishFile(t, w, "att-1", "a.txt", "mine\n")
	if err := w.artifacts.Defer(t.Context(), "p", m.ID, "test"); err != nil {
		t.Fatal(err)
	}

	now := w.service.landFor(t.Context(), parent, nil)(childWith(m.ID))
	if !strings.HasPrefix(now, "落地冲突：") || strings.Contains(now, "recovery:") || !strings.HasSuffix(now, ": a.txt"+applyHint) {
		t.Fatalf("landing text for an apply conflict recovery reached now = %q; want the conflict, why and its path", now)
	}
	if later := w.service.landFor(t.Context(), parent, nil)(childWith(m.ID)); later != now {
		t.Fatalf("landing text for the same apply conflict later = %q; reached now it was %q", later, now)
	}
}

// An apply conflict recovery reached in this pass says why without
// recovery's own cause, also once a result landed after it in the same
// pass has changed its path, so that its queue record holds it no longer.
func TestLandForSaysARecoveredApplyConflictWithoutItsCauseOnceItsPathMoves(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	var applied, editedAgain atomic.Bool
	var beforeOther atomic.Pointer[string]
	homeOnNode(t, w, func(req ops.Request) {
		if req.Op == ops.Apply && applied.CompareAndSwap(false, true) {
			editBy(t, w, "a.txt", "by hand\n")
		}
		// Edited again before the next landing snapshots the main
		// directory, a.txt changes in the canonical.
		if message := beforeOther.Load(); message != nil && req.Op == ops.Snapshot && req.Message == *message {
			editBy(t, w, "a.txt", "by hand again\n")
			editedAgain.Store(true)
		}
	})
	editBy(t, w, "a.txt", "base\n")
	m := publishFile(t, w, "att-1", "a.txt", "mine\n")
	other := publishFile(t, w, "att-2", "b.txt", "other\n")
	message := "before landing " + other.ID[:12]
	beforeOther.Store(&message)
	for _, r := range []artifact.Manifest{m, other} {
		if err := w.artifacts.Defer(t.Context(), "p", r.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}

	landing := w.service.landFor(t.Context(), parent, nil)
	if !editedAgain.Load() {
		t.Fatal("a.txt was not edited again before the later landing snapshotted the main directory")
	}
	if got := landing(childWith(other.ID)); !strings.HasPrefix(got, "已落地主目录") {
		t.Fatalf("landing text for the result landed after the conflict = %q", got)
	}
	if got := landing(childWith(m.ID)); !strings.HasPrefix(got, "落地冲突：") || strings.Contains(got, "recovery:") || !strings.HasSuffix(got, ": a.txt"+applyHint) {
		t.Fatalf("landing text for an apply conflict recovery reached before its path moved = %q; want the conflict, why and its path", got)
	}
}

// A result that did not land is one its parent can do nothing about: it
// cannot settle a conflict, nor land a result again. It is told what the
// user can do, and never the landing's internal state.
func TestLandForTellsAParentWhatComesOfAResultThatDidNotLand(t *testing.T) {
	for _, tc := range []struct {
		name string
		// queue queues the result the parent hears about, among what
		// stops it from landing.
		queue func(t *testing.T, w *world) artifact.Manifest
		// want is what the parent is told, once the pass is over.
		want func(t *testing.T, w *world) string
	}{{
		name: "merge conflict",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			_, first, second := conflictingResults(t, w)
			queueAll(t, w, first, second)
			return second
		},
		want: func(*testing.T, *world) string { return "落地冲突：notes.md" + mergeHint },
	}, {
		// With no marked tree, a merge conflict can be neither resolved
		// nor edited in the console.
		name: "merge conflict with no marked tree",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			_, first, second := unmarkedConflict(t, w)
			queueAll(t, w, first, second)
			return second
		},
		want: func(*testing.T, *world) string { return "落地冲突：notes.md" + noTreeHint },
	}, {
		name: "merge conflict with no marked tree, read from the queue",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			p, first, second := unmarkedConflict(t, w)
			queueAll(t, w, first, second)
			if _, err := w.artifacts.LandPending(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			stuck, err := w.artifacts.Stuck(t.Context(), "p")
			if err != nil || len(stuck) != 1 || stuck[0].Artifact != second.ID || stuck[0].Resolvable() {
				t.Fatalf("stuck results = %+v err=%v; want the one merge conflict, with no marked tree", stuck, err)
			}
			return second
		},
		want: func(*testing.T, *world) string { return "落地冲突：notes.md" + noTreeHint },
	}, {
		name: "apply conflict",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			m := nestedRepoResult(t, w)
			queueAll(t, w, m)
			return m
		},
		want: func(t *testing.T, w *world) string {
			stuck, err := w.artifacts.Stuck(t.Context(), "p")
			if err != nil || len(stuck) != 1 || !strings.Contains(stuck[0].Reason, "nested git repository (inner)") {
				t.Fatalf("stuck results = %+v err=%v; want the one apply conflict", stuck, err)
			}
			return "落地冲突：" + stuck[0].Reason + ": inner/lib.go" + applyHint
		},
	}, {
		// The landing can write neither the conflict it reached nor its
		// own close: it is left where it stopped.
		name: "conflict the landing could not record",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
			_, first, second := conflictingResults(t, w)
			if _, err := w.book.DB().Exec(`CREATE TRIGGER refuse_conflict BEFORE UPDATE ON operations
				WHEN NEW.kind = 'landing' AND NEW.state = 'merge-conflicted'
				BEGIN SELECT RAISE(ABORT, 'conflict not recorded'); END`); err != nil {
				t.Fatal(err)
			}
			queueAll(t, w, first, second)
			return second
		},
		want: func(*testing.T, *world) string { return unfinishedText },
	}, {
		// The landing cannot record the conflict it reached, but closes
		// itself as stopped before apply, with neither the paths nor the
		// marked tree of the conflict. The queue keeps both.
		name: "conflict the landing could only close",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
			_, first, second := conflictingResults(t, w)
			if _, err := w.book.DB().Exec(`CREATE TRIGGER refuse_conflict BEFORE UPDATE ON operations
				WHEN NEW.kind = 'landing' AND NEW.state = 'merge-conflicted' AND NEW.data NOT LIKE '%interrupted before apply%'
				BEGIN SELECT RAISE(ABORT, 'conflict not recorded'); END`); err != nil {
				t.Fatal(err)
			}
			queueAll(t, w, first, second)
			return second
		},
		want: func(t *testing.T, w *world) string {
			stuck, err := w.artifacts.Stuck(t.Context(), "p")
			if err != nil || len(stuck) != 1 || !stuck[0].Resolvable() || len(stuck[0].Paths) == 0 {
				t.Fatalf("stuck results = %+v err=%v; want the one merge conflict, with its paths and marked tree", stuck, err)
			}
			landings, err := w.artifacts.Landings(t.Context(), "p")
			if err != nil {
				t.Fatal(err)
			}
			closed := 0
			for _, l := range landings {
				if l.Artifact != stuck[0].Artifact {
					continue
				}
				if l.State != artifact.LandMergeConflicted || !l.Unapplied || l.Conflict != "" || len(l.Paths) != 0 {
					t.Fatalf("landing = %+v; want it closed before apply, with nothing of the conflict", l)
				}
				closed++
			}
			if closed != 1 {
				t.Fatalf("landings of the conflicted result = %d; want the one it closed", closed)
			}
			return unfinishedText
		},
	}, {
		// The landing records the conflict it reached, with its paths
		// and marked tree, but the queue does not keep it: neither the
		// console nor /resolve has it to act on.
		name: "merge conflict the queue could not keep",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
			_, first, second := conflictingResults(t, w)
			refuseQueuedConflicts(t, w)
			queueAll(t, w, first, second)
			return second
		},
		want: func(t *testing.T, w *world) string {
			if l := unqueuedConflict(t, w, artifact.LandMergeConflicted); l.Unapplied || l.Conflict == "" || len(l.Paths) == 0 {
				t.Fatalf("landing = %+v; want the merge conflict recorded, with its paths and marked tree", l)
			}
			return unfinishedText
		},
	}, {
		// An apply conflict found before apply wrote nothing either.
		name: "apply conflict the queue could not keep",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
			m := nestedRepoResult(t, w)
			refuseQueuedConflicts(t, w)
			queueAll(t, w, m)
			return m
		},
		want: func(t *testing.T, w *world) string {
			if l := unqueuedConflict(t, w, artifact.LandApplyConflicted); !l.Unapplied || !strings.Contains(l.ConflictReason(), "nested git repository (inner)") {
				t.Fatalf("landing = %+v; want the apply conflict recorded as found before apply", l)
			}
			return unfinishedText
		},
	}, {
		// An apply conflict recovery reached — a.txt edited by hand as
		// the landing starts writing it — comes once the landing has
		// begun writing the main directory.
		name: "apply conflict recovered the queue could not keep",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			var once sync.Once
			homeOnNode(t, w, func(req ops.Request) {
				if req.Op == ops.Apply {
					once.Do(func() { editBy(t, w, "a.txt", "by hand\n") })
				}
			})
			editBy(t, w, "a.txt", "base\n")
			m := publishFile(t, w, "att-1", "a.txt", "mine\n")
			refuseQueuedConflicts(t, w)
			queueAll(t, w, m)
			return m
		},
		want: func(t *testing.T, w *world) string {
			if l := unqueuedConflict(t, w, artifact.LandApplyConflicted); l.Unapplied || len(l.Paths) != 1 || l.Paths[0] != "a.txt" {
				t.Fatalf("landing = %+v; want the apply conflict recovery reached on a.txt", l)
			}
			return unfinishedWritingText
		},
	}, {
		// Retried once another result has moved the canonical on, the
		// result reaches its merge conflict again, but the queue keeps
		// the record of its earlier landing, not of this one.
		name: "merge conflict the queue kept of an earlier landing only",
		queue: func(t *testing.T, w *world) artifact.Manifest {
			ownStores(t, w, "", artifact.LocalNodes{Dir: t.TempDir()})
			p, first, second := conflictingResults(t, w)
			if land, err := w.artifacts.Land(t.Context(), p, first.ID, "test"); err != nil || land.State != artifact.LandCommitted {
				t.Fatalf("first landing = %+v err=%v", land, err)
			}
			var conflict artifact.Conflict
			if _, err := w.artifacts.Land(t.Context(), p, second.ID, "test"); !errors.As(err, &conflict) {
				t.Fatalf("second landing did not conflict: %v", err)
			}
			other := publishFile(t, w, "att-3", "other.md", "other\n")
			if land, err := w.artifacts.Land(t.Context(), p, other.ID, "test"); err != nil || land.State != artifact.LandCommitted {
				t.Fatalf("other landing = %+v err=%v", land, err)
			}
			refuseQueuedConflicts(t, w)
			return second
		},
		want: func(t *testing.T, w *world) string {
			stuck, err := w.artifacts.Stuck(t.Context(), "p")
			if err != nil || len(stuck) != 1 || !stuck[0].Resolvable() {
				t.Fatalf("stuck results = %+v err=%v; want the one merge conflict, with its marked tree", stuck, err)
			}
			landings, err := w.artifacts.Landings(t.Context(), "p")
			if err != nil {
				t.Fatal(err)
			}
			retried := 0
			for _, l := range landings {
				if l.Artifact != stuck[0].Artifact || l.ID == stuck[0].Landing {
					continue
				}
				if l.State != artifact.LandMergeConflicted || l.Unapplied || l.Conflict == "" {
					t.Fatalf("landing = %+v; want the retry's merge conflict recorded, with its marked tree", l)
				}
				retried++
			}
			if retried != 1 {
				t.Fatalf("landings of the conflicted result besides the queued one = %d; want the retry", retried)
			}
			return unfinishedText
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			parent := w.running(t, "codex")
			m := tc.queue(t, w)

			got := w.service.landFor(t.Context(), parent, nil)(childWith(m.ID))
			if want := tc.want(t, w); got != want {
				t.Fatalf("landing text = %q; want %q", got, want)
			}
		})
	}
}

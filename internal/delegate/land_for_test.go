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

	if got := w.service.landFor(t.Context(), parent, nil)(childWith(second.ID)); got != "落地冲突：notes.md" {
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
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md" {
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
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md" {
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
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md" {
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
	if got := landing(childWith(second.ID)); got != "落地冲突：notes.md" {
		t.Fatalf("landing text for a stuck result while the main directory is busy = %q; want the conflict and its path", got)
	}
	if got := landing(childWith(queued.ID)); got != busyText {
		t.Fatalf("landing text for a queued result while the main directory is busy = %q; want %q", got, busyText)
	}
}

// An apply conflict says why it stopped. Reached in this pass, it reads as
// the queue keeps it for every later pass.
func TestLandForSaysAnApplyConflictAsLaterPassesDo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	w := newWorld(t)
	parent := w.running(t, "codex")
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
	if err := w.artifacts.Defer(ctx, "p", m.ID, "test"); err != nil {
		t.Fatal(err)
	}

	now := w.service.landFor(ctx, parent, nil)(childWith(m.ID))
	if !strings.HasPrefix(now, "落地冲突：") || !strings.Contains(now, "nested git repository (inner)") || !strings.HasSuffix(now, ": inner/lib.go") {
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
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Node: "node-a", Path: home}}}); err != nil {
		t.Fatal(err)
	}
	nodes := hookedNodes{LocalNodes: artifact.LocalNodes{Dir: t.TempDir()}, before: before}
	w.artifacts, w.attempts, w.home = artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, nodes), attempt.New(book), home
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
	if !strings.HasPrefix(now, "落地冲突：") || strings.Contains(now, "recovery:") || !strings.HasSuffix(now, ": a.txt") {
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
	var applied atomic.Bool
	var beforeOther atomic.Pointer[string]
	homeOnNode(t, w, func(req ops.Request) {
		if req.Op == ops.Apply && applied.CompareAndSwap(false, true) {
			editBy(t, w, "a.txt", "by hand\n")
		}
		// Edited again before the next landing snapshots the main
		// directory, a.txt changes in the canonical.
		if message := beforeOther.Load(); message != nil && req.Op == ops.Snapshot && req.Message == *message {
			editBy(t, w, "a.txt", "by hand again\n")
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
	if got := landing(childWith(other.ID)); !strings.HasPrefix(got, "已落地主目录") {
		t.Fatalf("landing text for the result landed after the conflict = %q", got)
	}
	if got := landing(childWith(m.ID)); !strings.HasPrefix(got, "落地冲突：") || strings.Contains(got, "recovery:") || !strings.HasSuffix(got, ": a.txt") {
		t.Fatalf("landing text for an apply conflict recovery reached before its path moved = %q; want the conflict, why and its path", got)
	}
}

package delegate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
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
	ctx := t.Context()
	ws, err := w.artifacts.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.artifacts.CanonicalOf(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "notes.md"), []byte(body), 0o644); err != nil {
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

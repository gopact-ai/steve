package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

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

	got := w.service.landFor(t.Context(), parent, nil)(childWith(second.ID))
	if !strings.Contains(got, "落地冲突") || !strings.Contains(got, "notes.md") {
		t.Fatalf("landing text for a stuck result = %q; want the conflict and its path", got)
	}
}

// A conflict reached in this very pass reads the same as one reached
// earlier: by what it is, not by the landing's internal state name.
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
	got := landing(childWith(second.ID))
	if !strings.Contains(got, "落地冲突") || !strings.Contains(got, "notes.md") || strings.Contains(got, artifact.LandMergeConflicted) {
		t.Fatalf("landing text for a result that conflicted now = %q; want the conflict and its path", got)
	}
}

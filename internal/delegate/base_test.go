package delegate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
)

// A parent working in place hands its child what it has written so far:
// the child's base is the canonical workspace as it is now, cut under the
// parent's own canonical lock, not the snapshot the parent's turn began
// from.
func TestAChildOfAnInPlaceParentStartsFromWhatTheParentWrote(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "codex")
	turn, err := w.attempts.Open(t.Context(), attempt.Spec{TaskID: parent.ID, Kind: attempt.KindChat, Project: "p", Agent: "codex", Harness: "mock",
		Workspace: project.Workspace{ID: "canonical:p", Project: "p", Path: w.home, Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	held, ok := artifact.CanonicalLease(turn.Leases, "p")
	if !ok {
		t.Fatal("the in-place turn holds no canonical lock")
	}
	p, _, err := w.artifacts.Project(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.artifacts.SnapshotCanonicalUnder(t.Context(), p, held, "", turn.ID, "before turn"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.home, "draft.md"), []byte("by the parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, release := startBlocked(t, w, "codex")
	child, ok := w.tasks.Get(first.TaskID)
	_, seen := os.Stat(filepath.Join(child.Workspace, "draft.md"))
	release()
	waitForResult(t, w, first.TaskID)
	if !ok || child.Workspace == "" {
		t.Fatalf("child workspace unknown: %+v", child)
	}
	if seen != nil {
		t.Fatalf("the child does not see what its in-place parent wrote: %v", seen)
	}
}

package artifact

import (
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

func TestRetiredProjectKeepsReadOnlySnapshots(t *testing.T) {
	canonical := t.TempDir()
	write(t, canonical, "README", "historical source")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	snapshot, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "before retirement")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.projects.Reconcile(t.Context(), nil, "empty-declaration"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Project(t.Context(), p.ID); err != nil || found {
		t.Fatalf("retired project still active: %v, %v", found, err)
	}
	text, _, binary, _, err := store.File(t.Context(), p.ID, snapshot.ID, "README")
	if err != nil || binary || text != "historical source" {
		t.Fatalf("retirement lost read access: %q, %v", text, err)
	}
	if _, err := store.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "new-work"}); err == nil {
		t.Fatal("retired project admitted new execution")
	}
}

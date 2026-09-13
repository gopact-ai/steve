package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRepairsInterruptedInitializationBeforeTakingSnapshots(t *testing.T) {
	dir := t.TempDir()
	// An interrupted git init can leave HEAD before objects/config exist.
	write(t, dir, "HEAD", "ref: refs/heads/main\n")
	write(t, dir, "retained-evidence", "keep")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "answer", "recovered")
	id, _, err := repo.Snapshot(t.Context(), work, "", "recovered initialization", false)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy repositories with existing artifacts are upgraded in place.
	if err := os.Remove(filepath.Join(dir, "steve-initialized")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), dir)
	if err != nil || !reopened.Has(t.Context(), id) {
		t.Fatalf("lost existing artifact: %v", err)
	}
	if read(t, dir, "retained-evidence") != "keep" {
		t.Fatal("reinitialization discarded data")
	}
}

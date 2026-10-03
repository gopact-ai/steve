package gitrepo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
)

func TestRecoveryPathOperationUsesPhysicalAncestorInspection(t *testing.T) {
	repo, _, _, base, work := recoveryIntegrityFixture(t)
	incoming := t.TempDir()
	if err := repo.Checkout(t.Context(), base, incoming); err != nil {
		t.Fatal(err)
	}
	write(t, incoming, "incoming/new", "accepted\n")
	next, _, err := repo.Snapshot(t.Context(), incoming, base, "incoming", false)
	if err != nil {
		t.Fatal(err)
	}
	req := ops.Request{Op: ops.Kind("recovery_path_state"), Repo: repo.Dir, WorkTree: work, From: base, Commit: next, Path: "incoming/new"}
	safe, err := RunOperation(t.Context(), req)
	if err != nil || safe.State != "old" {
		t.Fatalf("safe recovery inspection: %+v %v", safe, err)
	}
	if err := os.Mkdir(filepath.Join(work, "kept-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kept-dir", filepath.Join(work, "incoming")); err != nil {
		t.Fatal(err)
	}
	blocked, err := RunOperation(t.Context(), req)
	if err != nil || blocked.State != "other" {
		t.Fatalf("physical ancestor inspection: %+v %v", blocked, err)
	}
}

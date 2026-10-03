package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRecoveryCleanupRefusesUnscopedOperationNames(t *testing.T) {
	repo, root, id, base, work := recoveryIntegrityFixture(t)
	for _, kind := range []ops.Kind{"inspect_recovery", "remove_recovery", "verify_recovery_content", "verify_recovery_remainder"} {
		request := ops.Request{Op: kind, Repo: repo.Dir, WorkTree: root, Path: filepath.Dir(work), Recovery: id, Commit: base, Generation: 1}
		if kind == "verify_recovery_content" || kind == "verify_recovery_remainder" {
			request.WorkTree = work
		}
		if err := validateOperation(request); err == nil || !strings.Contains(err.Error(), "unknown operation") {
			t.Errorf("unscoped operation was not refused by its contract: %s: %v", kind, err)
		}
	}
}

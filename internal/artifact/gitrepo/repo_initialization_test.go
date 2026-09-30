package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLeavesIncompleteExistingRepositoryUntouched(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "HEAD", "ref: refs/heads/main\n")
	write(t, dir, "retained-evidence", "keep")
	if _, err := Open(t.Context(), dir); err == nil {
		t.Fatal("accepted an incomplete existing repository")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 || read(t, dir, "HEAD") != "ref: refs/heads/main\n" || read(t, dir, "retained-evidence") != "keep" {
		t.Fatalf("changed the incomplete repository: %v, %v", entries, err)
	}
}

func TestOpenRecognizesCompleteRepositoryWithoutItsMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p.git")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "answer", "retained")
	id, _, err := repo.Snapshot(t.Context(), work, "", "retained artifact", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "retained-evidence", "keep")
	config := read(t, dir, "config")
	if err := os.Remove(filepath.Join(dir, "steve-initialized")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Has(t.Context(), id) || read(t, dir, "retained-evidence") != "keep" || read(t, dir, "config") != config {
		t.Fatal("recognizing the repository changed its data or configuration")
	}
	if _, err := os.Stat(filepath.Join(dir, "steve-initialized")); err != nil {
		t.Fatalf("initialization marker not restored: %v", err)
	}
}

func TestOpenDoesNotRepairAnotherWritersLockedRepository(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p.git")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "steve-initialized")); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "config.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.WriteString("another writer owns this lock\n"); err != nil {
		t.Fatal(err)
	}
	config, head := read(t, dir, "config"), read(t, dir, "HEAD")
	if _, err := Open(t.Context(), repo.Dir); err == nil {
		t.Fatal("accepted a repository held by another configuration writer")
	}
	if read(t, dir, "config") != config || read(t, dir, "HEAD") != head || read(t, dir, "config.lock") != "another writer owns this lock\n" {
		t.Fatal("changed another writer's repository or lock")
	}
	if _, err := os.Stat(filepath.Join(dir, "steve-initialized")); !os.IsNotExist(err) {
		t.Fatalf("marked a locked repository initialized: %v", err)
	}
}

func TestOpenDoesNotRewriteAnUnmarkedRepositoryConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p.git")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git(t.Context(), nil, "config", "user.name", "retained-owner"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "steve-initialized")); err != nil {
		t.Fatal(err)
	}
	config := read(t, dir, "config")
	if _, err := Open(t.Context(), dir); err == nil {
		t.Fatal("accepted an unmarked repository with different configuration")
	}
	if read(t, dir, "config") != config {
		t.Fatal("rewrote the existing repository configuration")
	}
}

func TestSnapshotDoesNotRecreateAMissingRepository(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p.git")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "answer", "retained")
	if _, _, err := repo.Snapshot(t.Context(), work, "", "missing repository", false); err == nil {
		t.Fatal("snapshot succeeded without a repository")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot created a partial repository instead of leaving it missing: %v", err)
	}
	reopened, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.Snapshot(t.Context(), work, "", "restored repository", false); err != nil {
		t.Fatalf("snapshot did not recover after reopening: %v", err)
	}
}

func TestCancelledOpenDoesNotPublishARepository(t *testing.T) {
	parent := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, filepath.Join(parent, "p.git")); err == nil {
		t.Fatal("cancelled initialization succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled initialization left directories: %v, %v", entries, err)
	}
}

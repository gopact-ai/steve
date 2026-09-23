package artifact

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The cached index only spares hashing; what a snapshot records is still
// the work tree against the parent it was given.
func TestSnapshotIndexCacheNeverHidesAChange(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	first, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	// Same size, same second: only git's racy check can see this.
	write(t, work, "a", "two")
	second, changed, err := repo.Snapshot(t.Context(), work, first, "second", false)
	if err != nil || !changed {
		t.Fatalf("same-size rewrite: %q %v %v", second, changed, err)
	}
	// Against an older parent the cache holds newer entries; the
	// snapshot must still describe the directory, not the cache.
	again, changed, err := repo.Snapshot(t.Context(), work, first, "again", false)
	if err != nil || !changed {
		t.Fatalf("older parent: %q %v %v", again, changed, err)
	}
	if paths, err := repo.Changed(t.Context(), second, again); err != nil || len(paths) != 0 {
		t.Fatalf("same directory, different trees: %v %v", paths, err)
	}
	if err := os.Remove(filepath.Join(work, "a")); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := repo.Snapshot(t.Context(), work, second, "gone", false); err != nil || !changed {
		t.Fatalf("deletion: %v %v", changed, err)
	}
}

func TestSnapshotIndexCorruptCacheFallsBack(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	parent, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	caches, _ := filepath.Glob(filepath.Join(repo.Dir, snapshotIndexDir, "*"))
	if len(caches) != 1 {
		t.Fatalf("want one cached index, have %v", caches)
	}
	if err := os.WriteFile(caches[0], []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, work, "b", "two")
	if _, changed, err := repo.Snapshot(t.Context(), work, parent, "second", false); err != nil || !changed {
		t.Fatalf("corrupt cache: %v %v", changed, err)
	}
}

func TestSnapshotIndexConcurrentSnapshotsOfOneDirectory(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	parent, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, work, "b", "two")
	var wg sync.WaitGroup
	shas := make([]string, 8)
	errs := make([]error, 8)
	for i := range shas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shas[i], _, errs[i] = repo.Snapshot(t.Context(), work, parent, "same", false)
		}()
	}
	wg.Wait()
	for i := range shas {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if paths, err := repo.Changed(t.Context(), parent, shas[i]); err != nil || len(paths) != 1 || paths[0] != "b" {
			t.Fatalf("snapshot %d: %v %v", i, paths, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(repo.Dir, snapshotIndexDir, "tmp-*")); len(left) != 0 {
		t.Fatalf("private indexes left behind: %v", left)
	}
}

// However old its name says it is — a snapshot that ran for hours, a
// clock that jumped after sleep — an index a snapshot is using is never
// swept: without it git would write an empty tree.
func TestSnapshotIndexSweepSparesIndexesInUse(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	index, _, cleanup, err := repo.snapshotIndex(work, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(index, []byte("in use"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(index)
	old := filepath.Join(dir, "tmp-1-abandoned")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotPruned.Delete(dir)
	repo.pruneSnapshotIndexes(dir, time.Now().Add(48*time.Hour))
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("an index in use was swept: %v", err)
	}
	if _, err := os.Stat(index + ".lock"); err != nil {
		t.Fatalf("the lock of an index in use was swept: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("an abandoned index was kept: %v", err)
	}
}

// A user's global git settings must not let the kept index skip looking
// at a file: fsmonitor and relaxed stat checks are pinned off.
func TestSnapshotIndexIgnoresGlobalTrustSettings(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\tcheckStat = minimal\n\ttrustctime = false\n\tuntrackedCache = true\n\tfsmonitor = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	parent, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, work, "a", "two")
	write(t, work, "b", "new")
	sha, changed, err := repo.Snapshot(t.Context(), work, parent, "second", false)
	if err != nil || !changed {
		t.Fatalf("second: %v %v", changed, err)
	}
	if paths, err := repo.Changed(t.Context(), parent, sha); err != nil || len(paths) != 2 {
		t.Fatalf("changed = %v %v", paths, err)
	}
}

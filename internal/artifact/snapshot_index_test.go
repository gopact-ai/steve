package artifact

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
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

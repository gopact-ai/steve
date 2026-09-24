package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two snapshots of the same content produce the same commit and pin the
// same ref. The one that finds the other's ref lock waits for it instead
// of failing.
func TestPinWaitsForAConcurrentPinOfTheSameCommit(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	sha, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	// The concurrent writer is still writing: the ref does not exist yet.
	if _, err := repo.Git(t.Context(), nil, "update-ref", "-d", RefFor(sha)); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo.Dir, RefFor(sha)+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		released <- os.Remove(lock)
	}()
	if err := repo.Pin(t.Context(), sha); err != nil {
		t.Fatalf("pin behind a briefly held lock: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	got, err := repo.Git(t.Context(), nil, "rev-parse", "--verify", RefFor(sha))
	if err != nil || strings.TrimSpace(got) != sha {
		t.Fatalf("ref = %q, %v; want %s", got, err, sha)
	}
}

// A commit whose artifact ref already names it is kept already. Pinning it
// again reads the ref and takes no ref lock, so even a lock left behind by
// a crashed git does not stand in the way.
func TestPinOfAPinnedCommitTakesNoRefLock(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a", "one")
	sha, _, err := repo.Snapshot(t.Context(), work, "", "first", false)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo.Dir, RefFor(sha)+".lock")
	if err := os.WriteFile(lock, []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := repo.Pin(t.Context(), sha); err != nil {
		t.Fatalf("pin of a pinned commit: %v", err)
	}
	if waited := time.Since(start); waited >= time.Second {
		t.Fatalf("pin of a pinned commit waited %v for the ref lock", waited)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("stale lock disturbed: %v", err)
	}
}

// BenchmarkPinPinnedCommit measures pinning a commit whose artifact ref
// already names it, as bundling and content replication do.
func BenchmarkPinPinnedCommit(b *testing.B) {
	repo, err := Open(b.Context(), filepath.Join(b.TempDir(), "objects.git"))
	if err != nil {
		b.Fatal(err)
	}
	work := b.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("one"), 0o644); err != nil {
		b.Fatal(err)
	}
	sha, _, err := repo.Snapshot(b.Context(), work, "", "first", false)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := repo.Pin(b.Context(), sha); err != nil {
			b.Fatal(err)
		}
	}
}

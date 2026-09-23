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
	lock := filepath.Join(repo.Dir, RefFor(sha)+".lock")
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

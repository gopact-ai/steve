package journal

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A journal nobody writes to has nothing to make durable, and an append
// reaches the disk within the sync interval.
func TestSyncFollowsWritesOnly(t *testing.T) {
	var syncs atomic.Int64
	j := newJournal(t, Options{SyncInterval: 2 * time.Millisecond, fsync: func(f *os.File) error {
		syncs.Add(1)
		return f.Sync()
	}})
	time.Sleep(50 * time.Millisecond)
	if n := syncs.Load(); n != 0 {
		t.Fatalf("idle journal synced %d times", n)
	}
	appendLine(t, j.Out, "one\n")
	waitUntil(t, "sync after an append", func() bool { return syncs.Load() > 0 })
	settled := syncs.Load()
	time.Sleep(50 * time.Millisecond)
	if n := syncs.Load(); n != settled {
		t.Fatalf("journal synced %d more times with no new writes", n-settled)
	}
	appendLine(t, j.In, "two\n")
	waitUntil(t, "sync after a second append", func() bool { return syncs.Load() > settled })
}

// fsync can take as long as the disk likes. Appends, which the process's
// live output waits on, must not wait for it.
func TestAppendDoesNotWaitForSync(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	j := newJournal(t, Options{SyncInterval: time.Millisecond, fsync: func(f *os.File) error {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
		return f.Sync()
	}})
	defer close(release)
	appendLine(t, j.Out, "one\n")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no sync started")
	}
	done := make(chan error, 1)
	go func() {
		_, err := j.Out.Append([]byte("two\n"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("append waited for fsync")
	}
}

package journal

import (
	"errors"
	"os"
	"strings"
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

// blockedSync makes the journal's first fsync wait for release and then
// return result(file); later fsyncs are real.
func blockedSync(result func(*os.File) error) (Options, <-chan struct{}, chan<- struct{}) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	return Options{SegmentBytes: 64, SyncInterval: time.Millisecond, fsync: func(f *os.File) error {
		if calls.Add(1) != 1 {
			return f.Sync()
		}
		close(entered)
		<-release
		return result(f)
	}}, entered, release
}

// A tick's fsync runs outside the lock, so rotation or Close can close its
// file meanwhile. Only the closed-file error that race produces is harmless:
// any other failure may be a writeback error the kernel reports once, which
// the rotation's or Close's own successful sync would then hide.
func TestSyncFailureOnAFileClosedMeanwhile(t *testing.T) {
	eio := errors.New("input/output error")
	long := strings.Repeat("x", 80) + "\n"
	t.Run("rotation, write-back error", func(t *testing.T) {
		opts, entered, release := blockedSync(func(*os.File) error { return eio })
		j := newJournal(t, opts)
		appendLine(t, j.Out, "one\n")
		<-entered
		appendLine(t, j.Out, long) // rotates, closing the file being synced
		close(release)
		waitUntil(t, "journal disabled", func() bool { return errors.Is(j.Err(), ErrUnresumable) })
		if !errors.Is(j.Err(), eio) {
			t.Fatal(j.Err())
		}
	})
	t.Run("rotation, closed file", func(t *testing.T) {
		opts, entered, release := blockedSync((*os.File).Sync)
		j := newJournal(t, opts)
		appendLine(t, j.Out, "one\n")
		<-entered
		appendLine(t, j.Out, long)
		close(release)
		appendLine(t, j.Out, "two\n")
		time.Sleep(20 * time.Millisecond)
		if err := j.Err(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("close, write-back error", func(t *testing.T) {
		opts, entered, release := blockedSync(func(*os.File) error { return eio })
		j, err := New(t.TempDir(), "stream-1", opts)
		if err != nil {
			t.Fatal(err)
		}
		appendLine(t, j.Out, "one\n")
		<-entered
		closed := make(chan error, 1)
		go func() { closed <- j.Close() }()
		// Close closes the files under the lock, then waits for the tick.
		waitUntil(t, "files closed", func() bool {
			j.mu.Lock()
			defer j.mu.Unlock()
			return j.Out.segments[0].file == nil
		})
		close(release)
		if err := <-closed; !errors.Is(err, ErrUnresumable) || !errors.Is(err, eio) {
			t.Fatalf("Close = %v", err)
		}
	})
}

package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// walBound is twice SQLite's default automatic checkpoint (1000 pages of
// 4 KiB): a WAL that keeps being reset stays below it.
const walBound = 2 * 1000 * 4096

// writeUnderReads commits about 12 MiB of distinct bindings while every
// pooled read connection loops over short reads, and returns the WAL size.
func writeUnderReads(t *testing.T, dir string, l *Ledger) int64 {
	t.Helper()
	ctx := t.Context()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range readConnections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := l.Bindings(context.Background(), "small"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	payload := strings.Repeat("p", 4000)
	for i := range 3000 {
		if err := l.PutBinding(ctx, "big", fmt.Sprint(i%200), fmt.Sprint(i, payload)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	info, err := os.Stat(filepath.Join(dir, databaseFile+"-wal"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// Pool reads overlap each other and the writer, but each is short, so the
// writer's automatic checkpoint keeps resetting the WAL. The held read is
// the control: one snapshot kept open pins the WAL and it grows with every
// commit, which is why reads must not stay open across slow work.
func TestShortOverlappingReadsLetTheWALReset(t *testing.T) {
	t.Run("short reads", func(t *testing.T) {
		dir := t.TempDir()
		l := open(t, dir, &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
		if err := l.PutBinding(t.Context(), "small", "a", "x"); err != nil {
			t.Fatal(err)
		}
		if size := writeUnderReads(t, dir, l); size > walBound {
			t.Fatalf("WAL grew to %d bytes under short reads, want at most %d", size, walBound)
		}
	})
	t.Run("held read", func(t *testing.T) {
		dir := t.TempDir()
		l := open(t, dir, &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
		if err := l.PutBinding(t.Context(), "small", "a", "x"); err != nil {
			t.Fatal(err)
		}
		release := holdRead(t, l, func(tx *ReadTx) error { _, err := bindingValue(tx, "small", "a"); return err })
		size := writeUnderReads(t, dir, l)
		if err := release(nil); err != nil {
			t.Fatal(err)
		}
		if size <= walBound {
			t.Fatalf("WAL stayed at %d bytes behind a held read; the bound no longer detects a pinned WAL", size)
		}
	})
}

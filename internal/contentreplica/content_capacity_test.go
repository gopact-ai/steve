package contentreplica_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestKnownReleaseCyclesBoundReceiverAndLedgerMetadata(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "known-only"
		if unknown {
			name = "with-old-unknown"
		}
		t.Run(name, func(t *testing.T) { exerciseReleaseCapacity(t, unknown) })
	}
}

func exerciseReleaseCapacity(t *testing.T, keepUnknown bool) {
	t.Helper()
	const limit = 32
	book := openBook(t)
	p, remote, client := newCluster(t, book, "internal", "a")
	dir := t.TempDir()
	cfg := contentreplica.StoreConfig{Dir: dir, NodeID: "a", Policy: p, Ledger: book, Limits: checkpoint.Limits{MaxObjects: limit, MaxBytes: 4096}}
	receiver, err := contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Close() }()
	remote.stores["a"] = receiver
	data := []byte("successful normal lifecycle")
	ref := checkpoint.Reference(data)
	unknownData := []byte("unreleased unknown promise")
	unknown := contentreplica.Upload{
		ID: "0000000000000001" + strings.Repeat("b", 48),
		Object: contentreplica.Object{
			Scope: p.scope, Kind: contentreplica.Material,
			Key: checkpoint.Reference(unknownData).SHA256, Blob: checkpoint.Reference(unknownData),
		},
	}
	var receipt contentreplica.Receipt
	if keepUnknown {
		receipt, err = receiver.Put(t.Context(), unknown, bytes.NewReader(unknownData))
		if err != nil {
			t.Fatal(err)
		}
	}
	var last contentreplica.Manifest
	for i := range 4 * limit {
		m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
		if err != nil {
			entries, _ := os.ReadDir(filepath.Join(dir, "retired"))
			t.Fatalf("cycle %d exhausted quota %d with %d retired markers: %v", i, limit, len(entries), err)
		}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			_, err := contentreplica.Record(tx, m)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, m.ID) }); err != nil {
			t.Fatal(err)
		}
		result, err := receiver.GC(t.Context(), book)
		if err != nil || result.Blobs != 1 {
			t.Fatalf("cycle %d release: %+v %v", i, result, err)
		}
		// Lose one reply, reopen, and require repeatable exact confirmation.
		if i%16 == 0 {
			if err := receiver.Close(); err != nil {
				t.Fatal(err)
			}
			receiver, err = contentreplica.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			remote.stores["a"] = receiver
			result, err = receiver.GC(t.Context(), book)
			if err != nil || result.Blobs != 0 || len(result.Released) != 1 {
				t.Fatalf("lost collection response: %+v %v", result, err)
			}
		}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			return contentreplica.ConfirmCollection(tx, "a", result.Released)
		}); err != nil {
			t.Fatal(err)
		}
		last = m
	}
	var rows int
	if err := book.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind LIKE 'content-%'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("completed ledger history was not compacted: rows=%d %v", rows, err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.Reserve(tx, contentreplica.Upload{ID: last.Receipts[0].UploadID, Object: last.Object}, []string{"a"})
	}); !errors.Is(err, contentreplica.ErrReleased) {
		t.Fatalf("pruned upload reopened publication admission: %v", err)
	}
	if _, err := receiver.Put(t.Context(), contentreplica.Upload{ID: last.Receipts[0].UploadID, Object: last.Object}, bytes.NewReader(data)); err == nil {
		t.Fatal("pruned upload replay was acknowledged")
	}
	if keepUnknown {
		replay, err := receiver.Put(t.Context(), unknown, bytes.NewReader(unknownData))
		if err != nil || !replay.StoredAt.Equal(receipt.StoredAt) || replay.Key() != receipt.Key() {
			t.Fatalf("old unknown was evicted by a newer floor: %+v %v", replay, err)
		}
	}
	files, size := contentFileUsage(t, dir)
	want := 1
	if keepUnknown {
		want = 3
	}
	if files != want || size > 2048 {
		t.Fatalf("completed markers still consume capacity: files=%d bytes=%d want=%d", files, size, want)
	}
	t.Logf("%d cycles with MaxObjects=%d MaxBytes=4096: %d files, %d bytes, %d content ledger row; unknown=%v", 4*limit, limit, files, size, rows, keepUnknown)
}

func contentFileUsage(t *testing.T, dir string) (int, int64) {
	t.Helper()
	var files int
	var size int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == "store.lock" {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		size += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, size
}

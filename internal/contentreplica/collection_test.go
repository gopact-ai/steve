package contentreplica_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

type pausedUploadReader struct {
	entered chan struct{}
	release chan struct{}
	source  io.Reader
}

func (r *pausedUploadReader) Read(p []byte) (int, error) {
	if r.entered != nil {
		close(r.entered)
		r.entered = nil
		<-r.release
	}
	return r.source.Read(p)
}

func TestConcurrentAbortCollectionWaitsForAdmittedStream(t *testing.T) {
	book := openBook(t)
	p, remote, _ := newCluster(t, book, "internal", "a")
	data := []byte("in flight after its exact release was committed")
	upload := sequenceUpload(p.scope, 1, data)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Reserve(tx, upload, []string{"a"}) }); err != nil {
		t.Fatal(err)
	}
	reader := &pausedUploadReader{entered: make(chan struct{}), release: make(chan struct{}), source: bytes.NewReader(data)}
	entered := reader.entered
	done := make(chan error, 1)
	receiver := remote.stores["a"]
	go func() {
		_, err := receiver.Put(t.Context(), upload, reader)
		done <- err
	}()
	<-entered
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, upload.ID) }); err != nil {
		close(reader.release)
		t.Fatal(err)
	}
	gc, err := receiver.GC(t.Context(), book)
	close(reader.release)
	if err != nil || len(gc.Released) != 0 || gc.Blobs != 0 {
		t.Fatalf("in-flight upload was prematurely confirmed/forgotten: %+v %v", gc, err)
	}
	if err := <-done; !errors.Is(err, checkpoint.ErrRetired) {
		t.Fatalf("released stream obtained a usable receipt: %v", err)
	}
	gc, err = receiver.GC(t.Context(), book)
	if err != nil || len(gc.Released) != 1 || gc.Blobs != 1 {
		t.Fatalf("finished stream lost its cleanup candidate: %+v %v", gc, err)
	}
	confirmReleased(t, book, "a", gc)
}

func sequenceUpload(scope contentreplica.Scope, seq uint64, data []byte) contentreplica.Upload {
	ref := checkpoint.Reference(data)
	return contentreplica.Upload{
		ID:     fmt.Sprintf("%016x%s", seq, strings.Repeat("b", 48)),
		Object: contentreplica.Object{Scope: scope, Kind: contentreplica.Material, Key: ref.SHA256, Blob: ref},
	}
}

func confirmReleased(t *testing.T, book *ledger.Ledger, node string, result checkpoint.RetentionGCResult) {
	t.Helper()
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.ConfirmCollection(tx, node, result.Released)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionWaitsForEveryExactTargetIncludingUnreceivedUploads(t *testing.T) {
	book := openBook(t)
	p, remote, _ := newCluster(t, book, "internal", "a", "b")
	data := []byte("only one target received these bytes")
	u := sequenceUpload(p.scope, 1, data)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Reserve(tx, u, []string{"a", "b"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.stores["a"].Put(t.Context(), u, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, u.ID) }); err != nil {
		t.Fatal(err)
	}
	a, err := remote.stores["a"].GC(t.Context(), book)
	if err != nil || len(a.Released) != 1 {
		t.Fatalf("receiver a: %+v %v", a, err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.ConfirmCollection(tx, "b", a.Released)
	}); !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("another node's exact acknowledgment accepted: %v", err)
	}
	confirmReleased(t, book, "a", a)
	var rows int
	if err := book.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind='content-upload'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("unreceived target was implicitly released: %d %v", rows, err)
	}
	b, err := remote.stores["b"].GC(t.Context(), book)
	if err != nil || b.Blobs != 0 || len(b.Released) != 1 || b.Released[0] == a.Released[0] {
		t.Fatalf("unreceived target must establish its own exact fence: %+v %v", b, err)
	}
	confirmReleased(t, book, "b", b)
	confirmReleased(t, book, "a", a) // lost final ledger response is an idempotent retry
	if err := book.DB().QueryRow(`SELECT count(*) FROM bindings WHERE kind LIKE 'content-%'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("completed upload history remains: %d %v", rows, err)
	}
	for _, receiver := range remote.stores {
		if _, err := receiver.Put(t.Context(), u, bytes.NewReader(data)); !errors.Is(err, checkpoint.ErrRetired) {
			t.Fatalf("pruned upload replay was admitted: %v", err)
		}
	}
}

func TestPrunedUploadCannotBecomeUnknownOnFreshReceiverStorage(t *testing.T) {
	book := openBook(t)
	p, remote, _ := newCluster(t, book, "internal", "a")
	data := []byte("an old ID does not become a new promise after disk replacement")
	u := sequenceUpload(p.scope, 1, data)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.ReleaseUnknown(tx, u, []string{"a"})
	}); err != nil {
		t.Fatal(err)
	}
	collected, err := remote.stores["a"].GC(t.Context(), book)
	if err != nil {
		t.Fatal(err)
	}
	confirmReleased(t, book, "a", collected)
	if err := remote.stores["a"].Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := contentreplica.Open(contentreplica.StoreConfig{Dir: t.TempDir(), NodeID: "a", Ledger: book, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := fresh.Put(t.Context(), u, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrReleased) {
		t.Fatalf("fresh receiver re-admitted a pruned original upload: %v", err)
	}
}

func TestFloorPreservesUnknownButRejectsUnreceivedOldPendingUpload(t *testing.T) {
	book := openBook(t)
	p, remote, _ := newCluster(t, book, "internal", "a")
	receiver := remote.stores["a"]
	data := []byte("old identities on either side of an admission boundary")
	unknown := sequenceUpload(p.scope, 1, data)
	receipt, err := receiver.Put(t.Context(), unknown, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	pendingData := []byte("unreceived pending upload of different bytes")
	pending := sequenceUpload(p.scope, 2, pendingData)
	released := sequenceUpload(p.scope, 3, data)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := contentreplica.Reserve(tx, pending, []string{"a"}); err != nil {
			return err
		}
		if err := contentreplica.Reserve(tx, released, []string{"a"}); err != nil {
			return err
		}
		return contentreplica.AbortUpload(tx, released.ID)
	}); err != nil {
		t.Fatal(err)
	}
	gc, err := receiver.GC(t.Context(), book)
	if err != nil || len(gc.Released) != 1 || gc.Blobs != 0 {
		t.Fatalf("release floor: %+v %v", gc, err)
	}
	confirmReleased(t, book, "a", gc)
	replayed, err := receiver.Put(t.Context(), unknown, bytes.NewReader(data))
	if err != nil || !replayed.StoredAt.Equal(receipt.StoredAt) {
		t.Fatalf("floor implicitly released unknown: %+v %v", replayed, err)
	}
	if _, err := receiver.Put(t.Context(), pending, bytes.NewReader(pendingData)); !errors.Is(err, checkpoint.ErrRetired) {
		t.Fatalf("unreceived old upload bypassed floor: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.AbortUpload(tx, pending.ID)
	}); err != nil {
		t.Fatal(err)
	}
	gc, err = receiver.GC(t.Context(), book)
	if err != nil || len(gc.Released) != 1 {
		t.Fatalf("explicitly closing refused old pending upload: %+v %v", gc, err)
	}
	confirmReleased(t, book, "a", gc)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.Reserve(tx, unknown, []string{"a"})
	}); !errors.Is(err, contentreplica.ErrReleased) {
		t.Fatalf("old unknown reopened publication: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.ReleaseUnknown(tx, unknown, []string{"a"})
	}); err != nil {
		t.Fatal(err)
	}
	gc, err = receiver.GC(t.Context(), book)
	if err != nil || gc.Blobs != 1 || len(gc.Released) != 1 {
		t.Fatalf("explicit old unknown release: %+v %v", gc, err)
	}
	confirmReleased(t, book, "a", gc)
}

func TestUnknownObjectQuotaExhaustionDoesNotEvictPromises(t *testing.T) {
	book := openBook(t)
	p, _, _ := newCluster(t, book, "internal", "a")
	dir := t.TempDir()
	cfg := contentreplica.StoreConfig{Dir: dir, NodeID: "a", Policy: p, Ledger: book, Limits: checkpoint.Limits{MaxObjects: 5}}
	store, err := contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	data := []byte("shared bytes still require independent receipt protection")
	var receipts []contentreplica.Receipt
	for i := uint64(1); i <= 3; i++ {
		r, err := store.Put(t.Context(), sequenceUpload(p.scope, i, data), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, r)
	}
	if _, err := store.Put(t.Context(), sequenceUpload(p.scope, 4, data), bytes.NewReader(data)); !errors.Is(err, checkpoint.ErrQuota) {
		t.Fatalf("object quota did not reject new unknown: %v", err)
	}
	if gc, err := store.GC(t.Context(), book); err != nil || len(gc.Released) != 0 || gc.Blobs != 0 {
		t.Fatalf("missing ledger rows were treated as release: %+v %v", gc, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, before := range receipts {
		r, err := store.Put(t.Context(), sequenceUpload(p.scope, uint64(i+1), data), bytes.NewReader(data))
		if err != nil || r.Key() != before.Key() || !r.StoredAt.Equal(before.StoredAt) {
			t.Fatalf("quota changed existing unknown: %+v %v", r, err)
		}
	}
	files, _ := contentFileUsage(t, dir)
	if files != 5 {
		t.Fatalf("quota accounting ignored markers/floor: %d", files)
	}
}

func TestReceiverFloorSurvivesOlderLedgerRestore(t *testing.T) {
	for _, state := range []string{"empty", "published"} {
		t.Run(state, func(t *testing.T) {
			book := openBook(t)
			p, remote, client := newCluster(t, book, "internal", "a")
			unknownData := []byte("unknown stays protected across authority rollback")
			unknown := sequenceUpload(p.scope, 1, unknownData)
			receiver := remote.stores["a"]
			before, err := receiver.Put(t.Context(), unknown, bytes.NewReader(unknownData))
			if err != nil {
				t.Fatal(err)
			}
			older, err := book.SnapshotReplica()
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("released before restoring an old ledger")
			ref := checkpoint.Reference(data)
			m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := contentreplica.Record(tx, m)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if state == "published" {
				older, err = book.SnapshotReplica()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, m.ID) }); err != nil {
				t.Fatal(err)
			}
			gc, err := receiver.GC(t.Context(), book)
			if err != nil || len(gc.Released) != 1 {
				t.Fatalf("release: %+v %v", gc, err)
			}
			confirmReleased(t, book, "a", gc)
			if err := book.RestoreReplica(older); err != nil {
				t.Fatal(err)
			}
			upload := contentreplica.Upload{ID: m.Receipts[0].UploadID, Object: m.Object}
			if _, err := receiver.Put(t.Context(), upload, bytes.NewReader(data)); !errors.Is(err, checkpoint.ErrRetired) {
				t.Fatalf("older %s ledger resurrected pruned exact receipt: %v", state, err)
			}
			after, err := receiver.Put(t.Context(), unknown, bytes.NewReader(unknownData))
			if err != nil || !after.StoredAt.Equal(before.StoredAt) {
				t.Fatalf("older ledger evicted existing unknown exception: %+v %v", after, err)
			}
		})
	}
}

package contentreplica_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestUploadPublicationAndReleaseHaveExactReplayFences(t *testing.T) {
	book := openBook(t)
	_, _, client := newCluster(t, book, "internal", "a")
	data := []byte("same bytes, independent promises")
	ref := checkpoint.Reference(data)
	a, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Receipts[0].UploadID == b.Receipts[0].UploadID || a.Receipts[0].Key() == b.Receipts[0].Key() {
		t.Fatal("content identity was incorrectly used as upload/receipt identity")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, a.Receipts[0].UploadID) }); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, a); return err }); !errors.Is(err, contentreplica.ErrReleased) {
		t.Fatalf("released upload replay was published: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, b); return err }); err != nil {
		t.Fatalf("independent upload was fenced by another upload: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, b.Receipts[0].UploadID) }); !errors.Is(err, contentreplica.ErrReferenced) {
		t.Fatalf("published content was abandoned as a pending upload: %v", err)
	}
}

func TestPendingBundleIntentPinsBaseUntilPublicationOrExplicitAbort(t *testing.T) {
	book := openBook(t)
	_, _, client := newCluster(t, book, "internal", "a")
	data := []byte("bundle bytes")
	ref := checkpoint.Reference(data)
	base, err := client("a").PrepareBundle(t.Context(), "p", strings.Repeat("1", 40), "", ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, base); return err }); err != nil {
		t.Fatal(err)
	}
	child, err := client("a").PrepareBundle(t.Context(), "p", strings.Repeat("2", 40), base.ID, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, base.ID) }); !errors.Is(err, contentreplica.ErrReferenced) {
		t.Fatalf("unknown bundle receipt lost its base: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, child.Receipts[0].UploadID) }); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, base.ID) }); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, child); return err }); err == nil {
		t.Fatal("late publication resurrected a retired dependency")
	}
	if _, err := client("a").PrepareBundle(t.Context(), "p", strings.Repeat("3", 40), base.ID, ref, bytes.NewReader(data)); err == nil {
		t.Fatal("new preparation bypassed base retirement")
	}
}

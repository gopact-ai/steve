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

func TestExactReleaseNeverCollectsAnotherUnknownUploadOfTheSameObject(t *testing.T) {
	book := openBook(t)
	p, remote, client := newCluster(t, book, "internal", "a")
	dir := t.TempDir()
	cfg := contentreplica.StoreConfig{Dir: dir, NodeID: "a", Policy: p, Ledger: book}
	receiver, err := contentreplica.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Close() }()
	remote.stores["a"] = receiver
	data := []byte("same object, unrelated unknown receipt")
	ref := checkpoint.Reference(data)
	a, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, a); return err }); err != nil {
		t.Fatal(err)
	}
	unknown := contentreplica.Upload{ID: strings.Repeat("b", 64), Object: a.Object}
	before, err := receiver.Put(t.Context(), unknown, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, a.ID) }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := receiver.GC(t.Context(), book)
		if err != nil || result.Blobs != 0 {
			t.Fatalf("unknown promise collected: %+v %v", result, err)
		}
		if err := receiver.Close(); err != nil {
			t.Fatal(err)
		}
		receiver, err = contentreplica.Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		after, err := receiver.Put(t.Context(), unknown, bytes.NewReader(data))
		if err != nil || before.Key() != after.Key() || !before.StoredAt.Equal(after.StoredAt) {
			t.Fatalf("unknown receipt was not stable across replay/restart: %+v %+v %v", before, after, err)
		}
		if _, err := receiver.Put(t.Context(), contentreplica.Upload{ID: a.Receipts[0].UploadID, Object: a.Object}, bytes.NewReader(data)); err == nil {
			t.Fatal("released upload replay obtained a receipt")
		}
	}
	// Explicit release-only adoption authorizes just this unknown key.
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.ReleaseUnknown(tx, unknown, []string{"a"})
	}); err != nil {
		t.Fatal(err)
	}
	result, err := receiver.GC(t.Context(), book)
	if err != nil || result.Blobs != 1 || result.Bytes != ref.Size {
		t.Fatalf("explicit release did not collect: %+v %v", result, err)
	}
	result, err = receiver.GC(t.Context(), book)
	if err != nil || result.Blobs != 0 {
		t.Fatalf("GC retry: %+v %v", result, err)
	}
}

func TestGCRejectsCorruptOwnerBeforeApplyingAnyRelease(t *testing.T) {
	book := openBook(t)
	_, remote, client := newCluster(t, book, "internal", "a")
	data := []byte("retained until the catalog is trustworthy")
	ref := checkpoint.Reference(data)
	m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.AbortUpload(tx, m.Receipts[0].UploadID) }); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES ('material','broken','{','test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.stores["a"].GC(t.Context(), book); !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("corrupt catalog accepted: %v", err)
	}
	var got bytes.Buffer
	if err := remote.stores["a"].Get(t.Context(), m.Object, &got); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("deleted before validating all roots: %v", err)
	}
}

func TestUnregisteredIncrementalUploadCannotCreateAnUnprotectedDependency(t *testing.T) {
	book := openBook(t)
	_, remote, _ := newCluster(t, book, "internal", "a")
	data := []byte("undeclared dependency")
	o := contentreplica.Object{Scope: contentreplica.Scope{ProjectID: "p", Level: "internal", HomeNodeID: "a"}, Kind: contentreplica.GitBundle, Key: strings.Repeat("1", 40), Base: strings.Repeat("2", 64), Blob: checkpoint.Reference(data)}
	if _, err := remote.stores["a"].Put(t.Context(), contentreplica.Upload{ID: strings.Repeat("3", 64), Object: o}, bytes.NewReader(data)); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("unprotected incremental receipt issued: %v", err)
	}
}

func TestGCMissingLiveReceiptMarkerCannotReleaseSharedBytes(t *testing.T) {
	for _, damage := range []string{"missing", "retired"} {
		t.Run(damage, func(t *testing.T) {
			book := openBook(t)
			p, remote, client := newCluster(t, book, "internal", "a")
			dir := t.TempDir()
			receiver, err := contentreplica.Open(contentreplica.StoreConfig{Dir: dir, NodeID: "a", Policy: p, Ledger: book})
			if err != nil {
				t.Fatal(err)
			}
			defer receiver.Close()
			remote.stores["a"] = receiver
			data := []byte("shared bytes still promised by a current receipt")
			ref := checkpoint.Reference(data)
			var current contentreplica.Manifest
			for range 2 {
				current, err = client("a").Prepare(t.Context(), "p", contentreplica.Material, ref.SHA256, ref, bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
					_, err := contentreplica.Record(tx, current)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := book.Update(t.Context(), contentreplica.ReleaseSuperseded); err != nil {
				t.Fatal(err)
			}
			name := current.Receipts[0].Key() + ".json"
			if damage == "missing" {
				err = os.Remove(filepath.Join(dir, "retained", name))
			} else {
				err = os.Rename(filepath.Join(dir, "retained", name), filepath.Join(dir, "retired", name))
			}
			if err != nil {
				t.Fatal(err)
			}
			if result, err := receiver.GC(t.Context(), book); !errors.Is(err, contentreplica.ErrIntegrity) && !errors.Is(err, checkpoint.ErrIntegrity) {
				t.Fatalf("GC accepted a missing live promise: %+v %v", result, err)
			}
			if err := receiver.Get(t.Context(), current.Object, &bytes.Buffer{}); err != nil {
				t.Fatalf("old receipt release removed current promised bytes: %v", err)
			}
		})
	}
}

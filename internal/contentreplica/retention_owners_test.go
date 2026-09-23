package contentreplica_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/plugins/pluginledger"
	"github.com/gopact-ai/steve/internal/project"
)

type retentionOwnerFixture struct {
	book     *ledger.Ledger
	receiver *contentreplica.Store
	key      string
	raw      string
	content  contentreplica.Manifest
	read     func() (*contentreplica.Manifest, error)
}

func newRetentionOwnerFixture(t *testing.T, kind string) retentionOwnerFixture {
	t.Helper()
	book := openBook(t)
	_, remote, client := newCluster(t, book, "internal", "a")
	f := retentionOwnerFixture{book: book, receiver: remote.stores["a"]}
	switch kind {
	case "artifact":
		data := []byte("durable artifact content")
		m, err := client("a").PrepareBundle(t.Context(), "p", strings.Repeat("1", 40), "", checkpoint.Reference(data), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		owner := artifact.Manifest{ID: m.Object.Key, Project: "p", Label: datalevel.Internal, CreatedAt: time.Now().UTC(), Content: &m}
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			if _, err := contentreplica.Record(tx, m); err != nil {
				return err
			}
			// The actual owner alone must protect content even without a
			// separate artifact-content restore index.
			return tx.PutBinding(kind, owner.ID, owner)
		}); err != nil {
			t.Fatal(err)
		}
		f.content = m
		store := artifact.New(t.TempDir(), book, project.Open(book), nil)
		f.read = func() (*contentreplica.Manifest, error) {
			got, found, err := store.Manifest(t.Context(), owner.ID)
			if err == nil && !found {
				err = errors.New("owner missing")
			}
			return got.Content, err
		}
	case "material":
		store, err := material.Open(t.TempDir(), book)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		store.SetReplication(client("a"))
		owner, err := store.Upload(t.Context(), "p", "retained", "text/plain", strings.NewReader("durable material content"))
		if err != nil {
			t.Fatal(err)
		}
		f.content = *owner.Content
		f.read = func() (*contentreplica.Manifest, error) {
			got, err := store.Get(t.Context(), "p", owner.ID)
			return got.Content, err
		}
	case "plugin-package":
		bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "examples/plugins/github"))
		if err != nil {
			t.Fatal(err)
		}
		library := &pluginledger.Library{Store: &plugins.Store{Dir: t.TempDir()}, Ledger: book, Replication: client("a")}
		owner, err := library.Add(t.Context(), "p", bundle)
		if err != nil {
			t.Fatal(err)
		}
		f.content = *owner.Content
		f.read = func() (*contentreplica.Manifest, error) {
			got, err := library.Record(t.Context(), "p", owner.Digest)
			return got.Content, err
		}
	default:
		t.Fatal("unknown test owner")
	}
	rows, err := book.Bindings(t.Context(), kind)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fixture owner rows=%d: %v", len(rows), err)
	}
	for key, raw := range rows {
		f.key, f.raw = key, string(raw)
	}
	return f
}

func (f retentionOwnerFixture) replace(t *testing.T, kind, raw string) {
	t.Helper()
	if err := f.book.PutBinding(t.Context(), kind, f.key, json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionProtectsEveryReferenceAcceptedByOwnerDecoders(t *testing.T) {
	for _, kind := range []string{"artifact", "material", "plugin-package"} {
		for _, field := range []string{
			`"content":`,
			`"Content":`,
			`"CONTENT":`,
			`"\u0043o\u006etent":`,
			`"content":null,"Content":`,
			`"content":null,"content":`,
		} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				f := newRetentionOwnerFixture(t, kind)
				f.replace(t, kind, strings.Replace(f.raw, `"content":`, field, 1))
				got, err := f.read()
				if err != nil || got == nil || got.ID != f.content.ID {
					t.Fatalf("fixture is not readable by real owner: %+v %v", got, err)
				}
				err = f.book.Update(t.Context(), func(tx *ledger.Tx) error {
					return contentreplica.Retire(tx, f.content.ID)
				})
				if !errors.Is(err, contentreplica.ErrReferenced) {
					t.Fatalf("retirement lost a reference accepted by the real owner: %v", err)
				}
				if _, err := f.receiver.GC(t.Context(), f.book); err != nil {
					t.Fatal(err)
				}
				if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
					t.Fatalf("referenced bytes were deleted: %v", err)
				}
			})
		}
	}
}

func TestArtifactRetentionOwnerAndRetirementShareOneTransaction(t *testing.T) {
	f := newRetentionOwnerFixture(t, "artifact")
	if err := f.book.DeleteBinding(t.Context(), "artifact", f.key); err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(f.raw, `"content":`, `"Content":`, 1)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := tx.PutBinding("artifact", f.key, json.RawMessage(raw)); err != nil {
			return err
		}
		if err := contentreplica.Retire(tx, f.content.ID); !errors.Is(err, contentreplica.ErrReferenced) {
			t.Fatalf("owner inserted in the same transaction was not protected: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		if _, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, "artifact", f.key); err != nil {
			return err
		}
		return contentreplica.Retire(tx, f.content.ID)
	}); err != nil {
		t.Fatalf("explicit owner removal and release did not share a transaction: %v", err)
	}
	result, err := f.receiver.GC(t.Context(), f.book)
	if err != nil || result.Blobs != 1 {
		t.Fatalf("committed owner removal did not authorize exact collection: %+v %v", result, err)
	}
}

func TestArtifactRetentionUsesOwnerDuplicatePointerSemantics(t *testing.T) {
	f := newRetentionOwnerFixture(t, "artifact")
	// encoding/json clears the pointer when the later case-folded field is
	// null. Looking only at the first canonical key would invent a root.
	f.replace(t, "artifact", strings.TrimSuffix(f.raw, "}")+`,"CONTENT":null}`)
	got, err := f.read()
	if err != nil || got != nil {
		t.Fatalf("fixture did not clear the real owner pointer: %+v %v", got, err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		return contentreplica.Retire(tx, f.content.ID)
	}); err != nil {
		t.Fatalf("retention decoder diverged from the whole owner decoder: %v", err)
	}
}

func TestRetentionRejectsUnicodeFoldedOwnerFieldTypeErrors(t *testing.T) {
	for kind, field := range map[string]string{
		"artifact":       "Meſſage",
		"material":       "ſize",
		"plugin-package": "digeſt",
	} {
		t.Run(kind, func(t *testing.T) {
			f := newRetentionOwnerFixture(t, kind)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(f.raw), &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "content")
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			// U+017F folds to S in encoding/json; these are real typed
			// owner fields, not harmless unknown metadata.
			f.replace(t, kind, strings.TrimSuffix(string(raw), "}")+`,"`+field+`":[]}`)
			if _, err := f.read(); err == nil {
				t.Fatal("fixture must fail the actual owner's Unicode-folded field decoder")
			}
			err = f.book.Update(t.Context(), func(tx *ledger.Tx) error {
				return contentreplica.Retire(tx, f.content.ID)
			})
			if !errors.Is(err, contentreplica.ErrIntegrity) {
				t.Fatalf("Unicode-folded type error was mistaken for no references: %v", err)
			}
		})
	}
}

func TestRetentionRejectsWholeOwnerDecodeErrorsBeforeRelease(t *testing.T) {
	for _, kind := range []string{"artifact", "material", "plugin-package"} {
		for _, field := range []string{`"project":`, `"PROJECT":`, `"\u0050roject":`} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				f := newRetentionOwnerFixture(t, kind)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(f.raw), &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "content")
				delete(fields, "project")
				raw, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				f.replace(t, kind, "{"+field+"123,"+string(raw[1:]))
				if _, err := f.read(); err == nil {
					t.Fatal("fixture must fail the actual whole-owner decoder")
				}
				err = f.book.Update(t.Context(), func(tx *ledger.Tx) error {
					return contentreplica.Retire(tx, f.content.ID)
				})
				if !errors.Is(err, contentreplica.ErrIntegrity) {
					t.Fatalf("invalid owner was treated as having no reference: %v", err)
				}
				if _, err := f.receiver.GC(t.Context(), f.book); !errors.Is(err, contentreplica.ErrIntegrity) {
					t.Fatalf("invalid owner did not close GC: %v", err)
				}
				if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
					t.Fatalf("GC removed bytes before validating the whole owner: %v", err)
				}
			})
		}
	}
}

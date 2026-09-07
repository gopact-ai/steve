package contentreplica_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
)

type places struct {
	scope   contentreplica.Scope
	domains map[string]string
	denied  map[string]bool
}

func (p *places) CheckpointPlacement(_ context.Context, scope contentreplica.Scope, node string) (contentreplica.Placement, error) {
	if scope != p.scope || p.denied[node] || p.domains[node] == "" || scope.Level == "sealed" && node != scope.HomeNodeID {
		return contentreplica.Placement{}, contentreplica.ErrPlacement
	}
	return contentreplica.Placement{FailureDomain: p.domains[node]}, nil
}

type transport struct {
	stores  map[string]*contentreplica.Store
	offline map[string]bool
	before  func(string, contentreplica.Object)
}

func (r *transport) Put(ctx context.Context, node string, object contentreplica.Object, source io.Reader) (contentreplica.Receipt, error) {
	if r.before != nil {
		r.before(node, object)
	}
	if r.offline[node] {
		return contentreplica.Receipt{}, errors.New("node offline")
	}
	return r.stores[node].Put(ctx, object, source)
}
func (r *transport) Get(ctx context.Context, node string, object contentreplica.Object, into io.Writer) error {
	if r.offline[node] {
		return errors.New("node offline")
	}
	return r.stores[node].Get(ctx, object, into)
}

func newCluster(t *testing.T, level string, names ...string) (*places, *transport, func(string) *contentreplica.Client) {
	t.Helper()
	p := &places{scope: contentreplica.Scope{ProjectID: "p", Level: level, HomeNodeID: "a"}, domains: map[string]string{}, denied: map[string]bool{}}
	r := &transport{stores: map[string]*contentreplica.Store{}, offline: map[string]bool{}}
	for _, name := range names {
		p.domains[name] = "machine-" + name
		store, err := contentreplica.Open(contentreplica.StoreConfig{Dir: t.TempDir(), NodeID: name, Policy: p})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		r.stores[name] = store
	}
	makeClient := func(name string) *contentreplica.Client {
		c, err := contentreplica.New(contentreplica.Config{NodeID: name, Local: r.stores[name], Remote: r, Policy: p, Scope: func(_ context.Context, id string) (contentreplica.Scope, error) {
			if id != "p" {
				return contentreplica.Scope{}, contentreplica.ErrPlacement
			}
			return p.scope, nil
		}, Members: func(context.Context) ([]string, error) { return names, nil }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return p, r, makeClient
}

func openBook(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

func TestMaterialWaitsForCopiesBeforeMetadataAndRestoresOnAnotherNode(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b", "c")
	book := openBook(t)
	a, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetReplication(client("a"))
	r.before = func(_ string, _ contentreplica.Object) {
		items, err := a.List(t.Context(), "p")
		if err != nil || len(items) != 0 {
			t.Fatal("material metadata visible before receiver receipt")
		}
	}
	m, err := a.Upload(t.Context(), "p", "note.txt", "text/plain", bytes.NewBufferString("survives the old coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Content == nil || !m.Content.Recoverable() || len(m.Content.Receipts) != 2 {
		t.Fatalf("material durability=%+v", m.Content)
	}
	r.before = nil
	r.offline["a"] = true
	b, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetReplication(client("c"))
	got, body, err := b.Content(t.Context(), "p", m.ID)
	if err != nil || got.ID != m.ID || string(body) != "survives the old coordinator" {
		t.Fatalf("restored material=%+v %q %v", got, body, err)
	}
	manifest, ok, err := contentreplica.Lookup(t.Context(), book, m.Content.ID)
	if err != nil || !ok || len(manifest.Receipts) != 3 {
		t.Fatalf("new content copy was not recorded: %+v %v", manifest, err)
	}
}

func TestMissingSecondCopyDoesNotAcknowledgeMaterial(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b")
	r.offline["b"] = true
	book := openBook(t)
	store, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetReplication(client("a"))
	if _, err := store.Upload(t.Context(), "p", "note", "text/plain", bytes.NewBufferString("not recoverable yet")); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("missing replica accepted: %v", err)
	}
	items, _ := store.List(t.Context(), "p")
	if len(items) != 0 {
		t.Fatal("failed upload published metadata")
	}
	manifests, _ := book.Bindings(t.Context(), contentreplica.ManifestKind)
	if len(manifests) != 0 {
		t.Fatal("failed upload published recovery manifest")
	}
}

func TestArtifactRestoresOriginalCommitAndBrowseAfterCoordinatorLoss(t *testing.T) {
	_, r, client := newCluster(t, "internal", "a", "b", "c")
	book := openBook(t)
	projects := project.Open(book)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "answer.txt"), []byte("original bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := project.Project{ID: "p", Home: project.Home{Path: work}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), "p")
	a := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	a.SetReplication(client("a"))
	snapshot, _, err := a.SnapshotCanonical(t.Context(), p, "", "worker", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Content == nil || !snapshot.Content.Recoverable() {
		t.Fatalf("artifact lacks content replicas: %+v", snapshot)
	}
	r.offline["a"] = true
	b := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	b.SetReplication(client("c"))
	text, _, _, truncated, err := b.File(t.Context(), "p", snapshot.ID, "answer.txt")
	if err != nil || truncated || text != "original bytes\n" {
		t.Fatalf("recovered artifact=%q %v", text, err)
	}
	repo, err := b.Repo(t.Context(), "p")
	if err != nil || !repo.Has(t.Context(), snapshot.ID) {
		t.Fatalf("original commit was not restored: %v", err)
	}
}

func TestSingleAndSealedContentDoNotClaimMachineLossProtection(t *testing.T) {
	for _, level := range []string{"internal", "sealed"} {
		t.Run(level, func(t *testing.T) {
			names := []string{"a"}
			if level == "sealed" {
				names = append(names, "b")
			}
			_, r, client := newCluster(t, level, names...)
			data := []byte("home only")
			m, err := client("a").Prepare(t.Context(), "p", contentreplica.Material, checkpoint.Reference(data).SHA256, checkpoint.Reference(data), bytes.NewReader(data))
			if err != nil || m.RequiredCopies != 1 || m.Recoverable() {
				t.Fatalf("single-home protection=%+v %v", m, err)
			}
			if level == "sealed" {
				var out bytes.Buffer
				if err := r.stores["b"].Get(t.Context(), m.Object, &out); !errors.Is(err, contentreplica.ErrPlacement) {
					t.Fatalf("sealed content left home: %v", err)
				}
			}
		})
	}
}

package contentreplica_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPluginLibraryRestoresExactPackageAfterCoordinatorLoss(t *testing.T) {
	_, transport, client := newCluster(t, "restricted", "a", "b", "c")
	book := openBook(t)
	bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "examples/plugins/github"))
	if err != nil {
		t.Fatal(err)
	}
	first := &plugins.Library{Store: &plugins.Store{Dir: filepath.Join(t.TempDir(), "plugins")}, Ledger: book, Replication: client("a")}
	transport.before = func(_ string, _ contentreplica.Object) {
		records, err := first.List(t.Context())
		if err != nil || len(records) != 0 {
			t.Fatal("package metadata committed before durable copies")
		}
	}
	record, err := first.Add(t.Context(), "p", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if record.Content == nil || !record.Content.Recoverable() {
		t.Fatal("package has no independent durable copy")
	}
	transport.before = nil
	transport.offline["a"] = true
	replacement := &plugins.Library{Store: &plugins.Store{Dir: filepath.Join(t.TempDir(), "plugins")}, Ledger: book, Replication: client("c")}
	restored, err := replacement.Get(t.Context(), "p", bundle.Digest)
	if err != nil || !bytes.Equal(restored.Data, bundle.Data) {
		t.Fatalf("restore original plugin bytes: %v", err)
	}
	if _, err := replacement.Get(t.Context(), "unrelated", bundle.Digest); err == nil {
		t.Fatal("cache presence bypassed project ownership")
	}
	current, found, err := contentreplica.Lookup(t.Context(), book, record.Content.ID)
	if err != nil || !found || len(current.Receipts) != 3 {
		t.Fatalf("repair receipt missing: %+v %v", current, err)
	}
}

func TestPluginLibraryDoesNotPublishWhenRequiredReplicaIsOffline(t *testing.T) {
	_, transport, client := newCluster(t, "restricted", "a", "b")
	transport.offline["b"] = true
	book := openBook(t)
	bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "examples/plugins/github"))
	if err != nil {
		t.Fatal(err)
	}
	library := &plugins.Library{Store: &plugins.Store{Dir: filepath.Join(t.TempDir(), "plugins")}, Ledger: book, Replication: client("a")}
	if _, err := library.Add(t.Context(), "p", bundle); err == nil {
		t.Fatal("insufficient copies acknowledged")
	}
	records, err := library.List(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("premature metadata: %+v %v", records, err)
	}
}

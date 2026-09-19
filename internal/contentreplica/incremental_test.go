package contentreplica_test

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/project"
)

func TestArtifactBundlesTransferOnlyChangesAndRestoreDependencyChain(t *testing.T) {
	book := openBook(t)
	_, remote, client := newCluster(t, book, "internal", "a", "b", "c")
	projects := project.Open(book)
	work := t.TempDir()
	data := make([]byte, 256<<10)
	if _, err := rand.New(rand.NewSource(7)).Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "unchanged.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := project.Project{ID: "p", Home: project.Home{Path: work}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	store := artifact.New(t.TempDir(), book, projects, nil)
	store.SetReplication(client("a"))
	first, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "base")
	if err != nil {
		t.Fatal(err)
	}
	parent := first
	for i, text := range []string{"delta one\n", "delta two\n"} {
		if err := os.WriteFile(filepath.Join(work, "changed.txt"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		next, _, err := store.SnapshotCanonical(t.Context(), p, parent.ID, "test", text)
		if err != nil {
			t.Fatal(err)
		}
		if next.Content.Object.Blob.Size >= first.Content.Object.Blob.Size/8 {
			t.Fatalf("delta %d retransmitted full closure: base=%d delta=%d", i, first.Content.Object.Blob.Size, next.Content.Object.Blob.Size)
		}
		raw, err := json.Marshal(next.Content.Object)
		if err != nil {
			t.Fatal(err)
		}
		var descriptor struct {
			Base string `json:"base"`
		}
		if err := json.Unmarshal(raw, &descriptor); err != nil || descriptor.Base != parent.Content.ID {
			t.Fatalf("dependency not bound to object identity: %s, %v", raw, err)
		}
		var bundle bytes.Buffer
		if err := remote.stores["b"].Get(t.Context(), next.Content.Object, &bundle); err != nil {
			t.Fatal(err)
		}
		header, _, _ := strings.Cut(bundle.String(), "\n\n")
		if !strings.Contains(header, "-"+parent.ID+" ") {
			t.Fatalf("bundle has no parent prerequisite: %q", header)
		}
		t.Logf("bundle bytes: full=%d incremental=%d", first.Content.Object.Blob.Size, next.Content.Object.Blob.Size)
		// Only the tip is in the restore index. A successful restore must
		// follow manifests, not rely on an earlier public-cache import.
		if err := book.DeleteBinding(t.Context(), "artifact-content", "p/"+parent.ID); err != nil {
			t.Fatal(err)
		}
		parent = next
	}
	again, changed, err := store.SnapshotCanonical(t.Context(), p, parent.ID, "test", "no changes")
	if err != nil || changed || again.Content.ID != parent.Content.ID {
		t.Fatalf("unchanged artifact changed its content representation: changed=%v err=%v", changed, err)
	}
	remote.offline["a"] = true
	replacement := artifact.New(t.TempDir(), book, projects, nil)
	replacement.SetReplication(client("c"))
	got, err := replacement.FileContent(t.Context(), "p", parent.ID, "unchanged.bin", int64(len(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("dependency restore did not recover original bytes: %v", err)
	}
	got, err = replacement.FileContent(t.Context(), "p", parent.ID, "changed.txt", 100)
	if err != nil || string(got) != "delta two\n" {
		t.Fatalf("dependency restore tip=%q: %v", got, err)
	}
	current, ok, err := contentreplica.Lookup(t.Context(), book, first.Content.ID)
	if err != nil || !ok || len(current.Receipts) != 3 {
		t.Fatalf("base repair receipt was not committed: %+v %v", current, err)
	}
}

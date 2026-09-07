package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestFileContentUsesPinnedSnapshotAndNeverTruncates(t *testing.T) {
	ctx := t.Context()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	root := t.TempDir()
	p := project.Project{ID: "sample", Home: project.Home{Path: root}}
	projects := project.Open(book)
	if err := projects.Declare(ctx, []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	store := New(t.TempDir(), book, projects, nil)
	blob := []byte{0, 1, 2, 3, 255}
	os.WriteFile(filepath.Join(root, "image.bin"), blob, 0o600)
	snap, _, err := store.SnapshotCanonical(ctx, p, "", "test", "base")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "image.bin"), []byte("changed"), 0o600)
	got, err := store.FileContent(ctx, p.ID, snap.ID, "image.bin", int64(len(blob)))
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("pinned binary=%v %v", got, err)
	}
	if _, err := store.FileContent(ctx, p.ID, snap.ID, "image.bin", 2); err == nil {
		t.Fatal("truncated capture accepted")
	}
	for _, name := range []string{"../image.bin", "/image.bin", "sub/../image.bin"} {
		if _, err := store.FileContent(ctx, p.ID, snap.ID, name, 10); err == nil {
			t.Fatalf("invalid reference accepted: %s", name)
		}
	}
	store.Review.Timeout = time.Nanosecond
	if _, err := store.FileContent(context.Background(), p.ID, snap.ID, "image.bin", 10); err == nil {
		t.Fatal("capture ignored read deadline")
	}
}

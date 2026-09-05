package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/project"
)

func TestSweepWorktreesTakesOnlyOldOrphans(t *testing.T) {
	node := &localNode{}
	store, _ := newStore(t, node, project.Home{Path: t.TempDir()})
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	mk := func(root, name string, age time.Time) string {
		dir := filepath.Join(root, "worktrees", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, age, age); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// Hub: an old orphan goes, an old live one stays, a fresh one stays.
	orphan := mk(store.Dir, "wt-aaaa-1", old)
	live := mk(store.Dir, "wt-aaaa-2", old)
	fresh := mk(store.Dir, "wt-aaaa-3", time.Now())
	mk(store.Dir, "notes", old) // not a worktree name: untouched
	removed, err := store.SweepWorktrees(ctx, "", "", func(p string) bool { return p == live })
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != orphan {
		t.Fatalf("removed = %v", removed)
	}
	for _, p := range []string{live, fresh, filepath.Join(store.Dir, "worktrees", "notes")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should have stayed: %v", p, err)
		}
	}
	// A machine: the same rules through its shell.
	root := t.TempDir()
	orphan2 := mk(root, "wt-bbbb-1", old)
	live2 := mk(root, "wt-bbbb-2", old)
	removed, err = store.SweepWorktrees(ctx, "node-x", root, func(p string) bool { return p == live2 })
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != orphan2 {
		t.Fatalf("node removed = %v", removed)
	}
	if _, err := os.Stat(live2); err != nil {
		t.Fatalf("live worktree on the node should have stayed: %v", err)
	}
}

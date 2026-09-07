package artifact

import (
	"context"
	"github.com/gopact-ai/steve/internal/project"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredProjectKeepsReadOnlySnapshots(t *testing.T) {
	canonical := t.TempDir()
	write(t, canonical, "README", "historical source")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	snapshot, _, err := store.SnapshotCanonical(t.Context(), p, "", "test", "before retirement")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.projects.Reconcile(t.Context(), nil, "empty-declaration"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Project(t.Context(), p.ID); err != nil || found {
		t.Fatalf("retired project still active: %v, %v", found, err)
	}
	text, _, binary, _, err := store.File(t.Context(), p.ID, snapshot.ID, "README")
	if err != nil || binary || text != "historical source" {
		t.Fatalf("retirement lost read access: %q, %v", text, err)
	}
	if _, err := store.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "new-work"}); err == nil {
		t.Fatal("retired project admitted new execution")
	}
}

func TestSourceTruncationPreservesUTF8Text(t *testing.T) {
	work := t.TempDir()
	contents := strings.Repeat("a", MaxFileBytes-1) + "中文"
	if err := os.WriteFile(filepath.Join(work, "large.txt"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	commit, _, err := repo.Snapshot(t.Context(), work, "", "large text", false)
	if err != nil {
		t.Fatal(err)
	}
	text, size, binary, truncated, err := repo.File(t.Context(), commit, "large.txt")
	if err != nil || binary || !truncated || len(text) != MaxFileBytes-1 || size != int64(len(contents)) {
		t.Fatalf("UTF-8 boundary became binary or corrupt: size=%d bytes=%d binary=%v truncated=%v err=%v", size, len(text), binary, truncated, err)
	}
}

func TestChangesIndexAndFileDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", "one\ntwo\n")
	write("gone.txt", "bye\n")
	before, _, err := repo.Snapshot(ctx, work, "", "before", false)
	if err != nil {
		t.Fatal(err)
	}
	write("keep.txt", "one\ntwo\nthree\n")
	write("new.go", "package x\n")
	if err := os.Remove(filepath.Join(work, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "pic.bin"), []byte{0, 1, 2, 0, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, err := repo.Snapshot(ctx, work, before, "after", false)
	if err != nil {
		t.Fatal(err)
	}
	changes, truncated, err := repo.Changes(ctx, before, after)
	if err != nil || truncated {
		t.Fatalf("changes: %v truncated=%v", err, truncated)
	}
	got := map[string]Change{}
	for _, c := range changes {
		got[c.Path] = c
	}
	if c := got["keep.txt"]; c.Status != "M" || c.Added != 1 || c.Deleted != 0 {
		t.Fatalf("keep.txt = %+v", c)
	}
	if c := got["new.go"]; c.Status != "A" || c.Added != 1 {
		t.Fatalf("new.go = %+v", c)
	}
	if c := got["gone.txt"]; c.Status != "D" || c.Deleted != 1 {
		t.Fatalf("gone.txt = %+v", c)
	}
	if c := got["pic.bin"]; c.Status != "A" || !c.Binary {
		t.Fatalf("pic.bin = %+v", c)
	}
	diff, cut, err := repo.FileDiff(ctx, before, after, "keep.txt")
	if err != nil || cut {
		t.Fatalf("diff: %v cut=%v", err, cut)
	}
	if !strings.Contains(diff, "+three") || strings.Contains(diff, "new.go") {
		t.Fatalf("diff = %q", diff)
	}
	// The first snapshot's "before" is the empty tree: everything is added.
	first, _, err := repo.Changes(ctx, "", before)
	if err != nil || len(first) != 2 || first[0].Status != "A" {
		t.Fatalf("first = %+v %v", first, err)
	}
}

package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<missing>"
	}
	return string(raw)
}

func TestSnapshotShadowsADirectoryWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "p.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	write(t, work, "a.txt", "one")
	write(t, work, "sub/b.txt", "two")
	first, changed, err := repo.Snapshot(ctx, work, "", "before")
	if err != nil || !changed {
		t.Fatalf("first snapshot = %s changed=%v err=%v", first, changed, err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); err == nil {
		t.Fatal("the user's directory grew a .git")
	}
	// Nothing changed: same commit back, no empty commit.
	again, changed, _ := repo.Snapshot(ctx, work, first, "again")
	if changed || again != first {
		t.Fatalf("unchanged snapshot = %s changed=%v", again, changed)
	}
	write(t, work, "a.txt", "one more")
	os.Remove(filepath.Join(work, "sub/b.txt"))
	write(t, work, "c.txt", "three")
	second, changed, _ := repo.Snapshot(ctx, work, first, "after")
	if !changed || second == first {
		t.Fatal("a change was not recorded")
	}
	paths, _ := repo.Changed(ctx, first, second)
	if len(paths) != 3 {
		t.Fatalf("changed = %v", paths)
	}
	parents, _ := repo.Parents(ctx, second)
	if len(parents) != 1 || parents[0] != first {
		t.Fatalf("parents = %v", parents)
	}
	// A checkout is the tree, exactly, into an empty directory only.
	out := filepath.Join(t.TempDir(), "wt")
	if err := repo.Checkout(ctx, second, out); err != nil {
		t.Fatal(err)
	}
	if read(t, out, "a.txt") != "one more" || read(t, out, "c.txt") != "three" || read(t, out, "sub/b.txt") != "<missing>" {
		t.Fatal("checkout does not match the snapshot")
	}
	if err := repo.Checkout(ctx, first, out); err == nil {
		t.Fatal("checkout into a non-empty directory was allowed")
	}
}

func TestBundleCarriesAClosureBetweenRepositories(t *testing.T) {
	ctx := context.Background()
	hub, _ := Open(ctx, filepath.Join(t.TempDir(), "hub.git"))
	node, _ := Open(ctx, filepath.Join(t.TempDir(), "node.git"))
	work := t.TempDir()
	write(t, work, "f", "1")
	base, _, _ := hub.Snapshot(ctx, work, "", "base")
	bundle := filepath.Join(t.TempDir(), "base.bundle")
	if err := hub.Bundle(ctx, bundle, base, nil); err != nil {
		t.Fatal(err)
	}
	if err := node.Unbundle(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	if !node.Has(ctx, base) {
		t.Fatal("the node did not receive the base")
	}
	// The node works on top and sends only the delta back.
	wt := filepath.Join(t.TempDir(), "wt")
	if err := node.Checkout(ctx, base, wt); err != nil {
		t.Fatal(err)
	}
	write(t, wt, "g", "2")
	result, changed, err := node.Snapshot(ctx, wt, base, "step")
	if err != nil || !changed {
		t.Fatalf("node snapshot = %s changed=%v err=%v", result, changed, err)
	}
	delta := filepath.Join(t.TempDir(), "result.bundle")
	if err := node.Bundle(ctx, delta, result, []string{base}); err != nil {
		t.Fatal(err)
	}
	if err := hub.Unbundle(ctx, delta); err != nil {
		t.Fatal(err)
	}
	if !hub.Has(ctx, result) {
		t.Fatal("the hub did not receive the result")
	}
}

func TestMergeAndApplyLandDisjointBranchesAndReportConflicts(t *testing.T) {
	ctx := context.Background()
	repo, _ := Open(ctx, filepath.Join(t.TempDir(), "p.git"))
	canonical := t.TempDir()
	write(t, canonical, "a", "a0")
	write(t, canonical, "b", "b0")
	base, _, _ := repo.Snapshot(ctx, canonical, "", "base")

	// Branch one edits a; the user meanwhile edits b in place.
	wt := filepath.Join(t.TempDir(), "wt")
	_ = repo.Checkout(ctx, base, wt)
	write(t, wt, "a", "a1")
	branch, _, _ := repo.Snapshot(ctx, wt, base, "branch")
	write(t, canonical, "b", "b-user")
	now, _, _ := repo.Snapshot(ctx, canonical, base, "now")

	merged, conflicts, err := repo.Merge(ctx, base, now, branch, "land")
	if err != nil || len(conflicts) != 0 || merged == "" {
		t.Fatalf("merge = %s conflicts=%v err=%v", merged, conflicts, err)
	}
	paths, err := repo.Apply(ctx, now, merged, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "a" || read(t, canonical, "a") != "a1" || read(t, canonical, "b") != "b-user" {
		t.Fatalf("apply touched %v; a=%q b=%q", paths, read(t, canonical, "a"), read(t, canonical, "b"))
	}
	// Both sides touch a: conflict, named, nothing applied.
	wt2 := filepath.Join(t.TempDir(), "wt2")
	_ = repo.Checkout(ctx, base, wt2)
	write(t, wt2, "a", "a-other")
	other, _, _ := repo.Snapshot(ctx, wt2, base, "other")
	_, conflicts, err = repo.Merge(ctx, base, merged, other, "land")
	if err != nil || len(conflicts) != 1 || conflicts[0] != "a" {
		t.Fatalf("conflicting merge = %v err=%v", conflicts, err)
	}
}

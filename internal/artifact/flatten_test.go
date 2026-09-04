package artifact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotFlattensAnAgentsNestedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, err := Open(context.Background(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	sub := filepath.Join(work, "hostline")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent ran git init inside its directory.
	if out, err := exec.Command("git", "-C", sub, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sha, changed, err := repo.Snapshot(context.Background(), work, "", "test")
	if err != nil || !changed {
		t.Fatalf("snapshot: %v changed=%v", err, changed)
	}
	tree, err := repo.git(context.Background(), nil, "ls-tree", "-r", "--name-only", sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tree, "hostline/main.go") {
		t.Fatalf("the agent's file did not reach the snapshot; tree:\n%s", tree)
	}
	if _, err := os.Stat(filepath.Join(sub, ".git")); !os.IsNotExist(err) {
		t.Fatal("the nested repository is still there")
	}
	script := Script{}.Snapshot("/objects.git", "/wt", "", "m")
	if !strings.Contains(script, "find . -mindepth 2 -name .git -prune -exec rm -rf {} +") {
		t.Fatalf("the node script does not flatten nested repositories:\n%s", script)
	}
}

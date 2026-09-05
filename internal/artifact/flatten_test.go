package artifact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
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
	sha, changed, err := repo.Snapshot(context.Background(), work, "", "test", true)
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
}

// A parent tree that already carries a gitlink — an earlier snapshot made
// before nested repositories were flattened — must not hide the files an
// agent writes under that path now, locally or through the typed node operation.
func TestSnapshotDropsAnInheritedGitlink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	// Craft the poisoned parent: a commit whose tree says hostline is a submodule.
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := repo.git(ctx, env, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",hostline"); err != nil {
		t.Fatal(err)
	}
	tree, err := repo.git(ctx, env, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := repo.git(ctx, nil, "commit-tree", strings.TrimSpace(tree), "-m", "poisoned")
	if err != nil {
		t.Fatal(err)
	}
	parent = strings.TrimSpace(parent)
	write := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "hostline"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "hostline", "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Locally.
	work := t.TempDir()
	write(work)
	sha, changed, err := repo.Snapshot(ctx, work, parent, "files", false)
	if err != nil || !changed {
		t.Fatalf("snapshot: %v changed=%v", err, changed)
	}
	if listing, _ := repo.git(ctx, nil, "ls-tree", "-r", "--name-only", sha); !strings.Contains(listing, "hostline/main.go") {
		t.Fatalf("local snapshot hid the files under the gitlink:\n%s", listing)
	}
	// Through the same typed implementation that a node runs.
	work2 := t.TempDir()
	write(work2)
	result, err := (LocalNodes{}).Artifact(ctx, "node", ops.Request{
		Op: ops.Snapshot, Repo: repo.Dir, WorkTree: work2, Parent: parent, Message: "files",
	})
	if err != nil || !result.Changed {
		t.Fatalf("typed snapshot: changed=%v, %v", result.Changed, err)
	}
	if listing, _ := repo.git(ctx, nil, "ls-tree", "-r", "--name-only", result.Commit); !strings.Contains(listing, "hostline/main.go") {
		t.Fatalf("typed snapshot hid the files under the gitlink:\n%s", listing)
	}
}

// A user's directory is never modified: a nested repository in it keeps
// its .git and is simply not part of the snapshot, locally and through
// the typed node operation.
func TestSnapshotLeavesAUsersNestedRepoAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	var referenceTree string
	for _, viaTyped := range []bool{false, true} {
		work := t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(work, "vendored")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", sub, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(sub, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var sha string
		if viaTyped {
			// Canonical directories may themselves be symlinks. The typed
			// walk must follow this root as the old remote cd did.
			link := filepath.Join(t.TempDir(), "workspace")
			if err := os.Symlink(work, link); err != nil {
				t.Fatal(err)
			}
			result, err := (LocalNodes{}).Artifact(ctx, "node", ops.Request{
				Op: ops.Snapshot, Repo: repo.Dir, WorkTree: link, Message: "m",
			})
			if err != nil {
				t.Fatal(err)
			}
			sha = result.Commit
		} else {
			sha, _, err = repo.Snapshot(ctx, work, "", "m", false)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(sub, ".git")); err != nil {
			t.Fatalf("typed=%v: the user's nested repository lost its .git: %v", viaTyped, err)
		}
		listing, _ := repo.git(ctx, nil, "ls-tree", "-r", "--name-only", sha)
		if !strings.Contains(listing, "notes.md") || strings.Contains(listing, "vendored") {
			t.Fatalf("typed=%v: tree = %q; want notes.md and no vendored entry", viaTyped, listing)
		}
		tree, err := repo.git(ctx, nil, "rev-parse", sha+"^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		if viaTyped && tree != referenceTree {
			t.Fatalf("typed tree %s differs from direct git tree %s", tree, referenceTree)
		}
		referenceTree = tree
	}
}

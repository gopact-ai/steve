package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

func snapshotWithOperation(t *testing.T, repo *Repo, work, parent string, flatten bool) (string, bool, error) {
	t.Helper()
	result, err := (LocalNodes{}).Artifact(t.Context(), "node", ops.Request{
		Op: ops.Snapshot, Repo: repo.Dir, WorkTree: work, Parent: parent,
		Message: "test", Flatten: flatten, Limits: ops.Limits(repo.Limits),
	})
	return result.Commit, result.Changed, err
}

func testSnapshot(t *testing.T, repo *Repo, work, parent string, flatten, typed bool) (string, bool, error) {
	t.Helper()
	if typed {
		return snapshotWithOperation(t, repo, work, parent, flatten)
	}
	return repo.Snapshot(t.Context(), work, parent, "test", flatten)
}

func TestSnapshotLimits(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, flatten := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				limits Limits
				want   TooLarge
			}{
				{"files", Limits{MaxFiles: 2}, TooLarge{"files", 3, 2}},
				{"bytes", Limits{MaxBytes: 8}, TooLarge{"bytes", 9, 8}},
				{"file_bytes", Limits{MaxFileBytes: 3}, TooLarge{"file_bytes", 4, 3}},
			} {
				t.Run(fmt.Sprintf("typed=%v/flatten=%v/%s", typed, flatten, tc.name), func(t *testing.T) {
					repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
					if err != nil {
						t.Fatal(err)
					}
					repo.Limits = tc.limits
					work := t.TempDir()
					write(t, work, "a file", "aa")
					write(t, work, "b\nfile", "bbb")
					write(t, work, "-file", "cccc")
					sha, changed, err := testSnapshot(t, repo, work, "", flatten, typed)
					var got TooLarge
					if !errors.As(err, &got) || got != tc.want || sha != "" || changed {
						t.Fatalf("snapshot: %q, %v, %v; want %+v", sha, changed, err, tc.want)
					}
					if !strings.Contains(err.Error(), "把大文件挪出工作区或加进 .gitignore") || !strings.Contains(err.Error(), fmt.Sprint(tc.want.Have)) || !strings.Contains(err.Error(), fmt.Sprint(tc.want.Limit)) {
						t.Fatalf("limit failure must explain the remedy and sizes: %v", err)
					}
					if out, err := repo.git(t.Context(), nil, "count-objects"); err != nil || !strings.HasPrefix(out, "0 objects") {
						t.Fatalf("rejected snapshot staged objects: %s, %v", out, err)
					}
					// Equality is allowed, in all three dimensions.
					repo.Limits = Limits{MaxFiles: 3, MaxBytes: 9, MaxFileBytes: 4}
					if _, changed, err := testSnapshot(t, repo, work, "", flatten, typed); err != nil || !changed {
						t.Fatalf("snapshot at the limit: changed=%v, %v", changed, err)
					}
				})
			}
		}
	}
}

func TestSnapshotLimitsRespectExclusions(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprintf("typed=%v", typed), func(t *testing.T) {
			repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
			if err != nil {
				t.Fatal(err)
			}
			repo.Limits = Limits{MaxFiles: 2, MaxBytes: 100, MaxFileBytes: 50}
			work := t.TempDir()
			write(t, work, ".gitignore", "ignored/\n")
			write(t, work, "kept", "ok")
			write(t, work, "ignored/large", strings.Repeat("x", 200))
			// A worktree's .git is a file, and must not hide siblings from
			// the directory walk or make the operation inspect git metadata.
			write(t, work, ".git", "gitdir: "+repo.Dir+"\n")
			nested := filepath.Join(work, "nested[1]\nrepo")
			write(t, nested, "large", strings.Repeat("x", 200))
			if out, err := exec.Command("git", "-C", nested, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("nested git init: %v, %s", err, out)
			}
			sha, _, err := testSnapshot(t, repo, work, "", false, typed)
			if err != nil {
				t.Fatal(err)
			}
			if listing, err := repo.git(t.Context(), nil, "ls-tree", "-r", "--name-only", sha); err != nil || strings.TrimSpace(listing) != ".gitignore\nkept" {
				t.Fatalf("excluded files entered the snapshot: %s, %v", listing, err)
			}
			if _, err := os.Stat(filepath.Join(nested, ".git")); err != nil {
				t.Fatalf("user's nested repository was touched: %v", err)
			}
			// Platform worktrees include flattened files, so a nested
			// repository cannot be used to evade their snapshot budgets.
			if _, _, err := testSnapshot(t, repo, work, "", true, typed); err == nil {
				t.Fatal("flattened files bypassed the limit")
			}
		})
	}
}

func TestSnapshotLimitsTrackedAndDeletedFiles(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprintf("typed=%v", typed), func(t *testing.T) {
			repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
			if err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			write(t, work, "tracked", "original")
			write(t, work, "deleted", "original")
			parent, _, err := testSnapshot(t, repo, work, "", false, typed)
			if err != nil {
				t.Fatal(err)
			}
			write(t, work, ".gitignore", "tracked\n")
			write(t, work, "tracked", strings.Repeat("x", 20))
			if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
				t.Fatal(err)
			}
			repo.Limits = Limits{MaxFiles: 2, MaxFileBytes: 10}
			_, _, err = testSnapshot(t, repo, work, parent, false, typed)
			var got TooLarge
			if !errors.As(err, &got) || got != (TooLarge{"file_bytes", 20, 10}) {
				t.Fatalf("tracked file was ignored, or deleted file counted: %v", err)
			}
		})
	}
}

func TestStorePreservesSnapshotTooLarge(t *testing.T) {
	for _, node := range []string{"", "node"} {
		t.Run("node="+node, func(t *testing.T) {
			n := &localNode{root: t.TempDir(), state: t.TempDir()}
			work := t.TempDir()
			store, p := newStore(t, n, project.Home{Node: node, Path: work})
			store.Limits = Limits{MaxFiles: 2}
			for _, name := range []string{"a", "b", "c"} {
				write(t, work, name, name)
			}
			for _, snapshot := range []func(context.Context) (Manifest, bool, error){
				func(ctx context.Context) (Manifest, bool, error) {
					return store.SnapshotCanonical(ctx, p, "", "test", "too large")
				},
				func(ctx context.Context) (Manifest, bool, error) {
					return store.SnapshotWorkspace(ctx, p, project.Workspace{ID: "copy", Project: p.ID, Node: node, Path: work, Kind: project.KindCopy}, "", "test", "too large")
				},
				func(ctx context.Context) (Manifest, bool, error) {
					return store.Publish(ctx, project.Workspace{Project: p.ID, Node: node, Path: work, Kind: project.KindWorktree}, "", "test", "too large")
				},
			} {
				_, changed, err := snapshot(t.Context())
				if err != (TooLarge{"files", 3, 2}) || changed {
					t.Fatalf("store swallowed or wrapped the limit: changed=%v, %v", changed, err)
				}
			}
			if head := store.CanonicalOf(t.Context(), p.ID); head != "" {
				t.Fatalf("failed snapshot moved the canonical head: %s", head)
			}
		})
	}
}

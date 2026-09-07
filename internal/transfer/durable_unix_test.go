//go:build unix

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestTransferDurabilitySyncsChildrenBeforeDirectoriesWithoutFollowingLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "installed")
	child := filepath.Join(root, "nested")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "result"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	// A dangling link must be persisted without trying to open its target.
	if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), filepath.Join(child, "outside")); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	if err := syncTransferPath(root, func(f *os.File) error {
		seen[f.Name()] = len(seen) + 1
		return f.Sync()
	}); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	child = filepath.Join(root, "nested")
	file := filepath.Join(child, "result")
	if seen[file] == 0 || seen[child] <= seen[file] || seen[root] <= seen[child] || seen[filepath.Dir(root)] <= seen[root] {
		t.Fatalf("children and new parent entries not synced before ancestors: %+v", seen)
	}
	if seen[filepath.Join(child, "outside")] != 0 {
		t.Fatal("symlink was opened during durability barrier")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := syncTransferPath(alias, (*os.File).Sync); err == nil {
		t.Fatal("imported root may not become a symlink")
	}
}

func TestTransferDoesNotActivateOwnershipUntilFileAndDirectorySyncSucceed(t *testing.T) {
	source, _ := transferFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	for _, failDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[failDirectory], func(t *testing.T) {
			target := t.TempDir()
			home := filepath.Join(t.TempDir(), "parent", "restored")
			finalized := false
			o := ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: bundle, Home: home, Finalize: func(project.Project) error { finalized = true; return nil }}
			failed := false
			failure := errors.New("injected durable write failure")
			_, err := importProject(t.Context(), o, func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() == failDirectory {
					failed = true
					return failure
				}
				return f.Sync()
			})
			if !failed || !errors.Is(err, failure) || !strings.Contains(err.Error(), "project remains inactive") || finalized {
				t.Fatalf("activation passed failed durability barrier: failed=%v finalized=%v err=%v", failed, finalized, err)
			}
			book, err := ledger.Open(target, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			owners := project.Open(book)
			_, found, err := owners.Ownership(t.Context(), "p")
			book.Close()
			if err != nil || found {
				t.Fatalf("failed sync persisted ownership: found=%v err=%v", found, err)
			}
			if _, err := Import(t.Context(), o); err != nil || !finalized {
				t.Fatalf("same-payload durability retry failed: finalized=%v err=%v", finalized, err)
			}
		})
	}
}

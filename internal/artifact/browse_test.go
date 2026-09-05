package artifact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeAndFileOfASnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "pkg", "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "pkg", "blob.bin"), []byte{1, 0, 2, 0, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	sha, _, err := repo.Snapshot(ctx, work, "", "s", false)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := repo.Tree(ctx, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, e := range root {
		kinds[e.Name] = e.Kind
	}
	if kinds["README.md"] != "file" || kinds["pkg"] != "dir" || kinds["link"] != "link" {
		t.Fatalf("root = %+v", root)
	}
	sub, _, err := repo.Tree(ctx, sha, "pkg/")
	if err != nil || len(sub) != 2 || sub[0].Path != "pkg/a.go" {
		t.Fatalf("pkg = %+v %v", sub, err)
	}
	text, size, binary, cut, err := repo.File(ctx, sha, "pkg/a.go")
	if err != nil || binary || cut || size != 12 || !strings.Contains(text, "package pkg") {
		t.Fatalf("a.go = %q size=%d binary=%v cut=%v err=%v", text, size, binary, cut, err)
	}
	if _, size, binary, _, err := repo.File(ctx, sha, "pkg/blob.bin"); err != nil || !binary || size != 5 {
		t.Fatalf("blob.bin binary=%v size=%d err=%v", binary, size, err)
	}
	if _, _, _, _, err := repo.File(ctx, sha, "pkg"); err == nil {
		t.Fatal("reading a directory as a file succeeded")
	}
	if _, _, _, _, err := repo.File(ctx, sha, "../etc/passwd"); err == nil {
		t.Fatal("a path outside the tree was accepted")
	}
}

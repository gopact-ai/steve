package node

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

func TestPlatformFileOperationsWithoutPOSIXTools(t *testing.T) {
	onlyGit(t)
	registry, _ := artifactNode(t)
	for _, name := range []string{"", "n"} {
		t.Run("node="+name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "probe ' $x\n[1]")
			if _, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileMkdir, Path: dir}); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("mkdir: %v", err)
			}
			if path, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileSearchPath}); err != nil || path != os.Getenv("PATH") {
				t.Fatalf("PATH: %q, %v", path, err)
			}
			artifactWrite(t, dir, "SKILL.md", "---\nname: demo\ndescription: example\n---\n# Demo\n")
			artifactWrite(t, dir, "scripts/run", "script")
			if err := os.Chmod(filepath.Join(dir, "scripts/run"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, excluded := range []string{".git/data", "nested/node_modules/data", "__pycache__/data"} {
				artifactWrite(t, dir, excluded, "excluded")
			}
			if err := os.Symlink("SKILL.md", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			archive, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileImportSkill, Path: dir})
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "imported")
			if err := skills.UnpackImport(archive, dest); err != nil {
				t.Fatal(err)
			}
			if skills.Describe(dest).Description != "example" {
				t.Fatal("archive lost the skill")
			}
			if info, err := os.Stat(filepath.Join(dest, "scripts/run")); err != nil || info.Mode()&0o111 == 0 {
				t.Fatalf("archive lost executable mode: %v", err)
			}
			for _, excluded := range []string{".git", "nested/node_modules", "__pycache__", "link"} {
				if _, err := os.Lstat(filepath.Join(dest, excluded)); !os.IsNotExist(err) {
					t.Fatalf("archive included %s: %v", excluded, err)
				}
			}
			// A real local git source needs no network, credentials or shell.
			source := t.TempDir()
			artifactWrite(t, source, "README", "cloned")
			for _, args := range [][]string{{"init", "--quiet"}, {"add", "README"}, {"-c", "user.name=test", "-c", "user.email=test@local", "commit", "--quiet", "-m", "source"}} {
				cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("source: %s: %v", out, err)
				}
			}
			clone := filepath.Join(t.TempDir(), "clone ' $x")
			if _, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileClone, Source: source, Path: clone}); err != nil {
				t.Fatal(err)
			}
			if body, err := os.ReadFile(filepath.Join(clone, "README")); err != nil || string(body) != "cloned" {
				t.Fatalf("clone: %q %v", body, err)
			}
			if _, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileClone, Source: source, Path: clone}); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing clone overwritten: %v", err)
			}
			failed := filepath.Join(t.TempDir(), "failed")
			if _, err := registry.Files(t.Context(), name, nodewire.FileRequest{Op: nodewire.FileClone, Source: filepath.Join(source, "absent"), Path: failed}); err == nil {
				t.Fatal("missing source accepted")
			}
			if entries, err := os.ReadDir(filepath.Dir(failed)); err != nil || len(entries) != 0 {
				t.Fatalf("failed clone left files: %v, %v", entries, err)
			}
		})
	}
}

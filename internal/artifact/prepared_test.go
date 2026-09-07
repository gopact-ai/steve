package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedWorkspaceVerificationChecksBytesWithoutReset(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "p.git"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	write(t, source, "nested/tracked", "original bytes")
	write(t, source, ".gitignore", "ignored\n")
	base, _, err := repo.Snapshot(t.Context(), source, "", "checkpoint", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unchanged", "content", "missing-file", "missing-directory", "extra", "ignored", "symlink-root", "symlink-file", "extra-directory"} {
		t.Run(name, func(t *testing.T) {
			var err error
			dir := filepath.Join(t.TempDir(), "prepared")
			if err := repo.Checkout(t.Context(), base, dir); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "content":
				write(t, dir, "nested/tracked", "modified bytes")
			case "missing-file":
				err = os.Remove(filepath.Join(dir, "nested/tracked"))
			case "missing-directory":
				err = os.RemoveAll(dir)
			case "extra":
				write(t, dir, "extra", "user data")
			case "ignored":
				write(t, dir, "ignored", "user data")
			case "symlink-root":
				link := filepath.Join(filepath.Dir(dir), "link")
				err = os.Symlink(dir, link)
				dir = link
			case "symlink-file":
				if err = os.Remove(filepath.Join(dir, "nested/tracked")); err == nil {
					err = os.Symlink(filepath.Join(source, "nested/tracked"), filepath.Join(dir, "nested/tracked"))
				}
			case "extra-directory":
				err = os.Mkdir(filepath.Join(dir, "extra-dir"), 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = repo.VerifyCheckout(t.Context(), base, dir)
			if name == "unchanged" {
				if err != nil {
					t.Fatalf("unchanged prepared checkout rejected: %v", err)
				}
			} else if !errors.Is(err, ErrPreparedWorkspaceChanged) {
				t.Fatalf("changed checkout accepted: %v", err)
			}
			if name == "content" && read(t, dir, "nested/tracked") != "modified bytes" {
				t.Fatal("verification reset user edits")
			}
			if name == "ignored" && read(t, dir, "ignored") != "user data" {
				t.Fatal("verification deleted ignored file")
			}
		})
	}
}

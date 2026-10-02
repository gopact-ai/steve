package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestRecoveryPreparationNeverResetsAnEmptyWorkingOrUnknownDirectory(t *testing.T) {
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "p.git"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	write(t, source, "tracked", "pinned base")
	base, _, err := repo.Snapshot(t.Context(), source, "", "baseline", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"prepared", "empty working", "unknown"} {
		t.Run(state, func(t *testing.T) {
			work := filepath.Join(t.TempDir(), "copy", "work")
			id := "workspace-recovery-" + strings.Repeat("a", 32)
			if state == "unknown" {
				if err := os.MkdirAll(work, 0700); err != nil {
					t.Fatal(err)
				}
				write(t, work, "unowned", "must survive")
			} else {
				if err := repo.PrepareRecovery(t.Context(), base, work, id); err != nil {
					t.Fatal(err)
				}
				if state == "empty working" {
					if err := os.Remove(filepath.Join(work, "tracked")); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := repo.PrepareRecovery(t.Context(), base, work, id)
			if state == "prepared" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrPreparedWorkspaceChanged) {
				t.Fatalf("late preparation accepted changed ownership or working bytes: %v", err)
			}
			if state == "empty working" {
				entries, err := os.ReadDir(work)
				if err != nil || len(entries) != 0 {
					t.Fatalf("late preparation restored the old base into working data: %v %v", entries, err)
				}
			}
			if state == "unknown" && read(t, work, "unowned") != "must survive" {
				t.Fatal("preparation deleted unknown content")
			}
		})
	}
}

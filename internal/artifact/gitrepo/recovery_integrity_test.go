package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recoveryIntegrityFixture(t *testing.T) (*Repo, string, string, string, string) {
	t.Helper()
	repo, err := Open(t.Context(), filepath.Join(t.TempDir(), "objects.git"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	write(t, source, "kept", "preserved\n")
	base, _, err := repo.Snapshot(t.Context(), source, "", "baseline", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	id := "workspace-recovery-" + strings.Repeat("1", 32)
	work := filepath.Join(root, "worktrees", "wt-"+base[:12]+"-"+id, "work")
	if err := repo.PrepareRecovery(t.Context(), base, work, id); err != nil {
		t.Fatal(err)
	}
	return repo, root, id, base, work
}

func TestRecoveryPathObservationRefusesUncapturedAncestors(t *testing.T) {
	for _, kind := range []string{"symlink", "file", "directory", "missing"} {
		t.Run(kind, func(t *testing.T) {
			repo, _, _, base, work := recoveryIntegrityFixture(t)
			incoming := t.TempDir()
			if err := repo.Checkout(t.Context(), base, incoming); err != nil {
				t.Fatal(err)
			}
			write(t, incoming, "incoming/new", "accepted\n")
			next, _, err := repo.Snapshot(t.Context(), incoming, base, "incoming", false)
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(work, "incoming")
			switch kind {
			case "symlink":
				if err := os.Mkdir(filepath.Join(work, "kept-dir"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("kept-dir", parent); err != nil {
					t.Fatal(err)
				}
			case "file":
				write(t, work, "incoming", "uncaptured\n")
			case "directory":
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			state, err := repo.pathState(t.Context(), work, base, next, "incoming/new")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" || kind == "file" {
				if state != "other" {
					t.Fatalf("uncaptured %s ancestor became writable: %s", kind, state)
				}
			} else if state != "old" {
				t.Fatalf("safe %s ancestor refused: %s", kind, state)
			}
			if kind == "symlink" {
				if target, err := os.Readlink(parent); err != nil || target != "kept-dir" {
					t.Fatalf("observation changed the ancestor: %q %v", target, err)
				}
			}
		})
	}
}

func TestRecoveryVerificationRefusesRootMetadataAndNonDirectoryRemainders(t *testing.T) {
	t.Run("root-metadata", func(t *testing.T) {
		repo, _, _, base, work := recoveryIntegrityFixture(t)
		write(t, work, ".git", "unpreserved metadata\n")
		if err := repo.VerifyRecoveryContent(t.Context(), base, work); err == nil {
			t.Fatal("excluded root metadata passed content preservation")
		}
		if read(t, work, ".git") != "unpreserved metadata\n" {
			t.Fatal("verification changed root metadata")
		}
	})
	t.Run("regular-root", func(t *testing.T) {
		repo, _, _, base, _ := recoveryIntegrityFixture(t)
		work := filepath.Join(t.TempDir(), "work")
		write(t, filepath.Dir(work), filepath.Base(work), "unpreserved root bytes\n")
		if err := repo.VerifyRecoveryRemainder(t.Context(), base, work); err == nil {
			t.Fatal("regular work root passed remainder preservation")
		}
		if read(t, filepath.Dir(work), filepath.Base(work)) != "unpreserved root bytes\n" {
			t.Fatal("verification changed root bytes")
		}
	})
}

func TestRecoveryContainerInspectionStaysInsideInstallationRoot(t *testing.T) {
	_, root, id, base, work := recoveryIntegrityFixture(t)
	container := filepath.Dir(work)
	observed, err := RecoveryContainer(t.Context(), root, container, id, base, "", "", false)
	if err != nil || !observed.Has {
		t.Fatalf("initial inspection: %+v %v", observed, err)
	}
	moved := filepath.Join(t.TempDir(), "owned-worktrees")
	if err := os.Rename(filepath.Join(root, "worktrees"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(root, "worktrees")); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoveryContainer(t.Context(), root, container, id, base, observed.Identity, observed.RootIdentity, false); err == nil {
		t.Fatal("rooted inspection followed a parent outside installation")
	}
	if read(t, moved, filepath.Join(filepath.Base(container), "work", "kept")) != "preserved\n" {
		t.Fatal("inspection changed relocated fixture content")
	}
}

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestMapWriteReportsCommittedDirectorySyncFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m, err := Open(DefaultPath(root))
	if err != nil {
		t.Fatal(err)
	}
	before := fileData{Sources: []Source{{Slug: "fixture"}}}
	if err := m.writeLocked(before); err != nil {
		t.Fatal(err)
	}
	failure := &os.PathError{Op: "sync", Path: root, Err: syscall.EIO}
	syncs := 0
	m.syncDir = func(dir string) error {
		syncs++
		if dir != root {
			t.Fatalf("synced %q, want %q", dir, root)
		}
		// Read through a fresh Map: the real write and rename must have
		// published the deletion before this fault is injected.
		reader, err := Open(m.path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reader.readLocked()
		if err != nil || len(got.Sources) != 0 {
			t.Fatalf("sync ran before deletion was published: %+v, %v", got, err)
		}
		return failure
	}
	err = m.writeLocked(fileData{})
	var committed *committedWriteError
	if !errors.As(err, &committed) || !errors.Is(err, failure) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("post-rename failure lost commit phase or cause: %v", err)
	}
	if syncs != 1 {
		t.Fatalf("directory syncs = %d, want 1", syncs)
	}
}

func TestMapRenameFailureIsNotCommitted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := DefaultPath(root)
	// Renaming the temporary regular file onto a directory must fail.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m.syncDir = func(string) error {
		t.Error("directory sync ran after a failed rename")
		return nil
	}
	err = m.writeLocked(fileData{})
	var committed *committedWriteError
	if err == nil || errors.As(err, &committed) {
		t.Fatalf("pre-commit failure misclassified: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) || !entries[0].IsDir() {
		t.Fatalf("rename failure altered destination or left a temp file: %v, %v", entries, err)
	}
}

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMapEnableDisableAndMaterialize(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind people")
	writeSkill(t, search, "weather", "weather")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("missing"); err == nil {
		t.Fatal("expected unknown skill")
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "remind" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
	dest := filepath.Join(root, "runtime")
	if err := m.Materialize(dest); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dest, "remind"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(search, "remind") {
		t.Fatalf("link = %q", target)
	}
	if _, err := os.Stat(filepath.Join(dest, "weather")); !os.IsNotExist(err) {
		t.Fatal("disabled skill was materialized")
	}
	if err := m.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("disabled skill still linked")
	}
}

func TestMapEnableAbsolutePath(t *testing.T) {
	root := t.TempDir()
	outside := writeSkill(t, t.TempDir(), "lark-im", "im")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(outside); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "lark-im" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestMapSearchPathAndAvailable(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	avail, err := m.Available()
	if err != nil {
		t.Fatal(err)
	}
	if len(avail) != 1 || avail[0].Name != "calendar" {
		t.Fatalf("available = %#v", avail)
	}
	if err := m.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePath(extra); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("calendar"); err == nil {
		t.Fatal("expected skill to disappear after path removal")
	}
}

func TestFingerprintStableAndOrdered(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "a", "a")
	writeSkill(t, search, "b", "b")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("a"); err != nil {
		t.Fatal(err)
	}
	first := m.Fingerprint()
	names := m.EnabledNames()
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("names = %v", names)
	}
	if m.Fingerprint() != first {
		t.Fatal("fingerprint unstable")
	}
	if err := m.Disable("a"); err != nil {
		t.Fatal(err)
	}
	if m.Fingerprint() == first {
		t.Fatal("fingerprint did not change")
	}
}

func TestMapDisableByBasenameAfterAbsEnable(t *testing.T) {
	root := t.TempDir()
	outside := writeSkill(t, t.TempDir(), "lark-im", "im")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(outside); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("lark-im"); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestMapEnableDedupesPathAndName(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(filepath.Join(search, "remind")); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 1 || enabled[0].Name != "remind" {
		t.Fatalf("enabled = %#v, %v", enabled, err)
	}
}

func TestRemovePathPrunesEnabled(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePath(extra); err != nil {
		t.Fatal(err)
	}
	enabled, err := m.Enabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("enabled after path removal = %#v, %v", enabled, err)
	}
}

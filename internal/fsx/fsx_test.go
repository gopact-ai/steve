package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileReplacesContentWithAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "new")
	assertOnly(t, dir, "state.json")
}

func TestWriteFileCreatesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "first")
	assertOnly(t, dir, "state.json")
}

func TestReplaceFileLeavesNoTemporaryFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	// A directory in the way makes the rename fail after the content is written.
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(target, []byte("data")); err == nil {
		t.Fatal("replaced a non-empty directory")
	}
	assertOnly(t, dir, "occupied")
}

func TestCreateFileNeverReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity")
	if err := CreateFile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "first")
	err := CreateFile(path, []byte("second"))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create = %v, want fs.ErrExist", err)
	}
	assertFile(t, path, "first")
	assertOnly(t, dir, "identity")
}

func TestSyncDirReportsAMissingDirectory(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("sync missing = %v, want fs.ErrNotExist", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v, want 0600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", path, raw, want)
	}
}

// assertOnly fails if a temporary file was left beside the published one.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if len(got) != len(names) {
		t.Fatalf("%s holds %v, want %v", dir, got, names)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Fatalf("%s holds %v, want %v", dir, got, names)
		}
	}
}

//go:build darwin

package acphost

import (
	"os"
	"path/filepath"
	"testing"
)

func openedTerminalImage(t *testing.T) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(path, []byte("original opened image"), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file, path
}

func replaceTerminalImage(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement image"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalExecutablePinSurvivesSourceReplacement(t *testing.T) {
	self, source := openedTerminalImage(t)
	pinned, release, err := pinTerminalExecutable(self, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	info, err := os.Stat(filepath.Dir(pinned))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("helper image is not in a private directory: %v %v", info, err)
	}
	original, _ := self.Stat()
	linked, _ := os.Stat(pinned)
	if !os.SameFile(original, linked) {
		t.Fatal("helper path did not pin the opened image")
	}
	replaceTerminalImage(t, source)
	raw, err := os.ReadFile(pinned)
	if err != nil || string(raw) != "original opened image" {
		t.Fatalf("source replacement redirected the helper: %q %v", raw, err)
	}
	release()
	if _, err := os.Lstat(filepath.Dir(pinned)); !os.IsNotExist(err) {
		t.Fatalf("private helper directory was not reclaimed: %v", err)
	}
}

func TestTerminalExecutablePinRejectsAlreadyReplacedSource(t *testing.T) {
	self, source := openedTerminalImage(t)
	replaceTerminalImage(t, source)
	pinned, release, err := pinTerminalExecutable(self, source)
	if err == nil || pinned != "" || release == nil {
		t.Fatalf("replacement became the trusted helper: %q %v", pinned, err)
	}
	// Failed publication already reclaimed its private files; retrying the
	// cleanup is harmless, and the replacement source remains untouched.
	release()
	raw, err := os.ReadFile(source)
	if err != nil || string(raw) != "replacement image" {
		t.Fatalf("cleanup changed the source path: %q %v", raw, err)
	}
}

func TestTerminalExecutableCopyReadsCapturedImage(t *testing.T) {
	self, source := openedTerminalImage(t)
	replaceTerminalImage(t, source)
	pinned := filepath.Join(t.TempDir(), "helper")
	if err := copyTerminalExecutable(self, pinned); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pinned)
	if err != nil || string(raw) != "original opened image" {
		t.Fatalf("cross-volume copy read the replacement pathname: %q %v", raw, err)
	}
	info, err := os.Stat(pinned)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("copied helper permissions differ: %v %v", info, err)
	}
}

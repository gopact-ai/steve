package filedoc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDocumentSavesPrivatelyAndLeavesNoTemporaryFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	doc := &Document{Path: filepath.Join(dir, "doc.json")}
	if raw, ok, err := doc.Load(); ok || err != nil || raw != nil {
		t.Fatalf("missing document loaded as %q %v %v", raw, ok, err)
	}
	for _, body := range []string{"first", "second"} {
		if err := doc.Save([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	raw, ok, err := doc.Load()
	if !ok || err != nil || string(raw) != "second" {
		t.Fatalf("Load = %q %v %v", raw, ok, err)
	}
	info, err := os.Stat(doc.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("document mode = %v %v", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %v %v, want only the document", entries, err)
	}
}

func TestDocumentSaveFailureLeavesThePreviousContent(t *testing.T) {
	dir := t.TempDir()
	doc := &Document{Path: filepath.Join(dir, "doc.json")}
	if err := doc.Save([]byte("kept")); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be makes the final rename fail.
	blocked := &Document{Path: filepath.Join(dir, "blocked")}
	if err := os.Mkdir(blocked.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked.Path, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blocked.Save([]byte("lost")); err == nil {
		t.Fatal("Save over a non-empty directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("a failed save left %d entries", len(entries))
	}
	if raw, _, _ := doc.Load(); string(raw) != "kept" {
		t.Fatalf("other document changed to %q", raw)
	}
}

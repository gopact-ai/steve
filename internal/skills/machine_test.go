package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanLocalAndImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "skills", "notes")
	_ = os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: notes\ndescription: keep notes\n---\n# notes\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	// A second tool linking to the first's directory lists nothing new.
	_ = os.Symlink(filepath.Join(home, ".codex", "skills"), filepath.Join(home, ".agents"))
	found := ScanLocal(home)
	real, _ := filepath.EvalSymlinks(dir)
	if len(found) != 1 || found[0].Name != "notes" || found[0].Description != "keep notes" || found[0].Path != real {
		t.Fatalf("found = %+v", found)
	}
	encoded, err := PackImport(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "notes")
	if err := UnpackImport(encoded, dest); err != nil {
		t.Fatal(err)
	}
	if Describe(dest).Description != "keep notes" {
		t.Fatal("SKILL.md did not arrive")
	}
	if info, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("script lost its bit: %v", err)
	}
	if err := UnpackImport(encoded, dest); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("overwrote: %v", err)
	}
	if err := UnpackImport("not base64!", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestUnpackImportClearsAnEarlierAttemptFirst(t *testing.T) {
	src := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	encoded, err := PackImport(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "notes")
	stale := filepath.Join(dest+".loading", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UnpackImport(encoded, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "stale.txt")); err == nil {
		t.Fatal("an earlier attempt's file was merged into the import")
	}
	if os.Getuid() == 0 {
		t.Skip("root removes anything")
	}
	dest = filepath.Join(t.TempDir(), "notes")
	locked := filepath.Join(dest+".loading", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := UnpackImport(encoded, dest); err == nil || !strings.Contains(err.Error(), "clear an earlier import") {
		t.Fatalf("leftovers that will not go were merged: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a skill was installed on top of leftovers")
	}
}

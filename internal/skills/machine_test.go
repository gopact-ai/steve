package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanAndImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "skills", "notes")
	_ = os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: notes\ndescription: keep notes\n---\n# notes\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	// A second tool linking to the first's directory lists nothing new.
	_ = os.Symlink(filepath.Join(home, ".codex", "skills"), filepath.Join(home, ".agents"))
	_ = os.MkdirAll(filepath.Join(home, ".agents"), 0o755)
	cmd := exec.Command("/bin/sh", "-c", ScanScript())
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scan: %v: %s", err, out)
	}
	found := ParseScan(string(out))
	real, _ := filepath.EvalSymlinks(dir)
	if len(found) != 1 || found[0].Name != "notes" || found[0].Description != "keep notes" || found[0].Path != real {
		t.Fatalf("found = %+v (%s)", found, out)
	}
	cmd = exec.Command("/bin/sh", "-c", ImportScript(dir))
	encoded, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "notes")
	if err := UnpackImport(string(encoded), dest); err != nil {
		t.Fatal(err)
	}
	if Describe(dest).Description != "keep notes" {
		t.Fatal("SKILL.md did not arrive")
	}
	if info, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("script lost its bit: %v", err)
	}
	if err := UnpackImport(string(encoded), dest); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("overwrote: %v", err)
	}
	if err := UnpackImport("not base64!", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("garbage accepted")
	}
}

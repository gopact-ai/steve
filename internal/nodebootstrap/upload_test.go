package nodebootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUploadUsesPrivateExclusiveStagingAndRejectsInvalidID(t *testing.T) {
	home := t.TempDir()
	id := strings.Repeat("a", 48)
	payload := "binary bytes\x00\xff$(touch /must-not-run)"
	command, err := UploadCommand(id)
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string) ([]byte, error) {
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	if output, err := run(payload); err != nil {
		t.Fatalf("upload failed: %s %v", output, err)
	}
	path := filepath.Join(home, "steve-bin", ".upload-"+id, "steve-node")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != payload {
		t.Fatalf("binary stream corrupted: %q %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("upload mode = %v", info.Mode())
	}
	if _, err := run("replacement"); err == nil {
		t.Fatal("repeated upload overwrote existing staging")
	}
	for _, id := range []string{"../escape", "'; touch /must-not-run", ""} {
		if _, err := UploadCommand(id); err == nil {
			t.Fatalf("accepted unsafe upload ID %q", id)
		}
	}
	cleanup, err := CleanupUploadCommand(strings.Repeat("a", 48))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", cleanup)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup failed: %s %v", output, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("cleanup retained upload")
	}
}

func TestUploadedBootstrapRefusesTamperedBinaryBeforeCreatingConfig(t *testing.T) {
	home := t.TempDir()
	id := strings.Repeat("b", 48)
	dir := filepath.Join(home, "steve-bin", ".upload-"+id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "steve-node"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := sample()
	sum := sha256.Sum256([]byte("intended binary"))
	spec.UploadID, spec.SHA256, spec.OS, spec.Arch = id, hex.EncodeToString(sum[:]), runtime.GOOS, runtime.GOARCH
	script, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "curl") || strings.Contains(script, "https://") {
		t.Fatal("upload bootstrap retained HTTP download dependency")
	}
	cmd := exec.Command("bash")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "SHA-256 verification failed") {
		t.Fatalf("tampered upload accepted: %s %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(home, "steve-bin", "node.json")); !os.IsNotExist(err) {
		t.Fatal("tampered upload created config")
	}
}

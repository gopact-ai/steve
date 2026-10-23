package nodebootstrap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func peerSpec() PeerSpec {
	return PeerSpec{UploadID: strings.Repeat("a", 48), OS: "linux", Arch: "amd64", SHA256: strings.Repeat("b", 64), JoinPackage: base64.StdEncoding.EncodeToString([]byte(`{"operation_id":"example","private_key":"test-private-key"}`))}
}

func TestPeerScriptKeepsJoinSecretsInStdinAndHasNoNodeRegistrationSideEffects(t *testing.T) {
	spec := peerSpec()
	script, err := BuildPeer(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "curl") || strings.Contains(script, "/console/nodes") || strings.Contains(script, "pgrep") {
		t.Fatal("peer bootstrap retained node-only registration or broad process operations")
	}
	if !strings.Contains(script, "peer-import --package") || !strings.Contains(script, "peer --config") || !strings.Contains(script, "umask 077") {
		t.Fatal("peer bootstrap omitted private import or runtime")
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, spec.JoinPackage) && line != spec.JoinPackage {
			t.Fatal("join package used outside stdin data block")
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid script %s %v", output, err)
	}
	for _, bad := range []PeerSpec{{UploadID: "../../bad"}, {UploadID: spec.UploadID, OS: "linux", Arch: "amd64", SHA256: spec.SHA256, JoinPackage: "not-base64;$(bad)"}} {
		if _, err := BuildPeer(bad); err == nil {
			t.Fatal("invalid peer bootstrap data accepted")
		}
	}
}

func TestPeerScriptRefusesExistingLegacyNodeBeforeChangingFiles(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "steve-bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(bin, "node.json")
	if err := os.WriteFile(config, []byte("existing-node"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := BuildPeer(peerSpec())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "already exists") {
		t.Fatalf("legacy node wasn't refused: %s %v", output, err)
	}
	if data, _ := os.ReadFile(config); string(data) != "existing-node" {
		t.Fatal("legacy configuration overwritten")
	}
}

func TestPeerScriptImportsPrivatePackageAndStartsOnlyInstalledProgram(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer script requires Unix")
	}
	home := t.TempDir()
	id := strings.Repeat("e", 48)
	staging := filepath.Join(home, "steve-bin", ".upload-"+id)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	program := `#!/bin/sh
case "$1" in
peer-import)
  [ "$2" = '--package' ] && [ "$4" = '--state-dir' ] || exit 41
  [ ! -e "$5" ] || exit 42
  mkdir -p "$5"
  cat "$3" > "$5/imported.json"
  printf '%s' "$*" > "$5/import-args"
  exit 0
  ;;
peer)
  printf '%s' "$*" > "$HOME/.steve-peer/run-args"
  printf '%s' "$$" > "$HOME/.steve-peer/started.pid"
  exec sleep 15
  ;;
esac
exit 43
`
	if err := os.WriteFile(filepath.Join(staging, "steve-node"), []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(program))
	payload := []byte(`{"private_key":"private-test-value","operation_id":"reviewed"}`)
	script, err := BuildPeer(PeerSpec{UploadID: id, OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), JoinPackage: base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bash")
	command.Env = append(os.Environ(), "HOME="+home)
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("peer script failed: %s %v", output, err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		pidData, readErr := os.ReadFile(filepath.Join(home, ".steve-peer", "started.pid"))
		if readErr == nil {
			pid, _ := strconv.Atoi(string(pidData))
			if pid > 0 {
				process, _ := os.FindProcess(pid)
				t.Cleanup(func() { _ = process.Kill() })
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated peer did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	imported, err := os.ReadFile(filepath.Join(home, ".steve-peer", "imported.json"))
	if err != nil || string(imported) != string(payload) {
		t.Fatal("private package did not arrive unchanged")
	}
	info, _ := os.Stat(filepath.Join(home, ".steve-peer", "imported.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private package mode %v", info.Mode())
	}
	for _, name := range []string{"import-args", "run-args"} {
		args, _ := os.ReadFile(filepath.Join(home, ".steve-peer", name))
		if strings.Contains(string(args), "private-test-value") || strings.Contains(string(args), base64.StdEncoding.EncodeToString(payload)) {
			t.Fatal("package secret appeared in process arguments")
		}
	}
	if strings.Contains(string(output), "private-test-value") {
		t.Fatal("package secret appeared in installer output")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("peer installation retained sensitive staging")
	}
}

package nodebootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sample() Spec {
	return Spec{Name: "remote", Port: "7701", Token: "node-secret", Harnesses: map[string]Harness{"example": {Command: "example", Args: []string{"--stdio"}}}}
}

func TestBootstrapRefusesExistingNodeAndPreservesItsFiles(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "steve-bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "node.json")
	if err := os.WriteFile(path, []byte("existing installation"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := Build(sample())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "already exists") {
		t.Fatalf("existing node was not refused: %s, %v", output, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "existing installation" {
		t.Fatalf("existing config overwritten: %s", got)
	}
}

func TestBootstrapHasNoSecretsInProgramArgumentsOrUnsafeInterpolation(t *testing.T) {
	spec := sample()
	spec.DownloadURL = "https://coordinator.example/dist/steve-node?token=node-secret"
	spec.OS, spec.Arch, spec.SHA256 = "linux", "amd64", strings.Repeat("a", 64)
	spec.Harnesses["example"] = Harness{Command: "program'with$chars", Args: []string{"\nSTEVE_CONFIG\n$(touch /must-not-run)"}}
	script, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "pgrep") || strings.Contains(script, "kill \"") || strings.Contains(script, "curl -fsSL '") {
		t.Fatal("bootstrap retained broad process termination or credential arguments")
	}
	if !strings.Contains(script, "curl -q --config -") || !strings.Contains(script, "set -C") || !strings.Contains(script, "umask 077") {
		t.Fatal("bootstrap lacks secure download or exclusive config creation")
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script is not valid bash: %s, %v", out, err)
		}
	}
	for _, url := range []string{"https://u:secret@example.test/file", "https://example.test/\ncommand", "file:///tmp/node", "https://example.test/file#fragment"} {
		spec.DownloadURL = url
		if _, err := Build(spec); err == nil {
			t.Fatalf("accepted unsafe download URL %q", url)
		}
	}
}

func TestInspectBinaryUsesHeadersRatherThanExecuting(t *testing.T) {
	// A minimal ELF64 little endian header is enough to identify the target.
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	path := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InspectBinary(path)
	if err != nil || got.OS != "linux" || got.Arch != "amd64" || len(got.SHA256) != 64 {
		t.Fatalf("binary metadata = %#v, %v", got, err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a binary"), 10), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBinary(path); err == nil {
		t.Fatal("accepted unknown executable format")
	}
}

func TestBootstrapDownloadAndChecksumInIsolatedHome(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("bootstrap requires a Unix platform")
	}
	for _, command := range []string{"bash", "curl", "nohup"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip(command + " is unavailable")
		}
	}
	// The isolated executable records only its own PID and sleeps. Cleanup
	// terminates that exact process; no agent, real node or global process runs.
	payload := []byte("#!/bin/bash\nprintf '%s' \"$$\" > \"$HOME/started.pid\"\nexec sleep 30\n")
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "download-secret" {
			t.Error("download did not use the supplied credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	for _, mismatch := range []bool{true, false} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			home := t.TempDir()
			spec := sample()
			spec.DownloadURL = server.URL + "/dist/steve-node?token=download-secret"
			spec.OS, spec.Arch, spec.SHA256 = runtime.GOOS, runtime.GOARCH, hex.EncodeToString(sum[:])
			if mismatch {
				spec.SHA256 = strings.Repeat("0", 64)
			}
			script, err := Build(spec)
			if err != nil {
				t.Fatal(err)
			}
			script = strings.Replace(script, "node_pid=$!", "node_pid=$!\nprintf '%s' \"$node_pid\" > \"$HOME/launched.pid\"", 1)
			// Isolate the login-shell boundary as well as HOME. The production
			// script deliberately uses a login shell, but a test must never load
			// the developer account's profile or wait on its startup hooks.
			bash, _ := exec.LookPath("bash")
			commands := filepath.Join(home, "commands")
			if err := os.Mkdir(commands, 0o700); err != nil {
				t.Fatal(err)
			}
			wrapper := "#!/bin/sh\nexec \"$HOME/steve-bin/steve-node\" -config \"$HOME/steve-bin/node.json\"\n"
			if err := os.WriteFile(filepath.Join(commands, "bash"), []byte(wrapper), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), bash)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+commands+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Stdin = strings.NewReader(script)
			output, err := cmd.CombinedOutput()
			configPath := filepath.Join(home, "steve-bin", "node.json")
			if mismatch {
				if err == nil || !strings.Contains(string(output), "SHA-256 verification failed") {
					t.Fatalf("checksum mismatch accepted: %s, %v", output, err)
				}
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatal("bad download created node configuration")
				}
				return
			}
			pidData, launchedErr := os.ReadFile(filepath.Join(home, "launched.pid"))
			if launchedErr == nil {
				pid, _ := strconv.Atoi(string(pidData))
				if pid > 0 {
					process, _ := os.FindProcess(pid)
					t.Cleanup(func() { _ = process.Kill() })
				}
			}
			var readErr error
			for deadline := time.Now().Add(5 * time.Second); ; {
				_, readErr = os.ReadFile(filepath.Join(home, "started.pid"))
				if readErr == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || readErr != nil {
				log, _ := os.ReadFile(filepath.Join(home, "steve-node.log"))
				t.Fatalf("installation did not start test process: %s, %v, %v; isolated process log: %s", output, err, readErr, log)
			}
			info, err := os.Stat(configPath)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("configuration permissions = %v, %v", info, err)
			}
			data, _ := os.ReadFile(configPath)
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil || config["token"] != "node-secret" {
				t.Fatalf("invalid configuration: %v", err)
			}
			if strings.Contains(string(output), "secret") {
				t.Fatal("installation printed a credential")
			}
			if _, err := os.Stat(filepath.Join(home, "steve-bin", ".install-lock")); !os.IsNotExist(err) {
				t.Fatal("completed installation retained its lock")
			}
		})
	}
}

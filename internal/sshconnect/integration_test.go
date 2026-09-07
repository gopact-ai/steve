package sshconnect

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

func TestMain(m *testing.M) {
	// The uploaded native test executable acts only as a bounded harmless
	// process. Its isolated HOME is supplied by the SSH fixture below.
	if os.Getenv("STEVE_SSH_TEST_PROCESS") == "1" {
		_ = os.WriteFile(filepath.Join(os.Getenv("HOME"), "started.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(15 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type shellBackend struct {
	path string
	home string
}

type isolatedShellRunner struct{ OpenSSH }

func (r isolatedShellRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r.OpenSSH, args: append([]string{}, args...)}, nil
}

func (b shellBackend) spec(req InstallRequest, token, id string) (nodebootstrap.Spec, nodebootstrap.Binary, error) {
	metadata, err := nodebootstrap.InspectBinary(b.path)
	_, port, _ := net.SplitHostPort(req.Addr)
	return nodebootstrap.Spec{Name: req.Name, Port: port, Token: token, UploadID: id, OS: metadata.OS, Arch: metadata.Arch, SHA256: metadata.SHA256, Harnesses: map[string]nodebootstrap.Harness{}}, metadata, err
}

func (b shellBackend) Preview(_ context.Context, req InstallRequest, _ CheckResult) (Template, error) {
	spec, metadata, err := b.spec(req, PreviewToken, nodebootstrap.PreviewUploadID)
	if err != nil {
		return Template{}, err
	}
	script, err := nodebootstrap.Build(spec)
	return Template{Script: script, BinaryPath: b.path, Binary: &metadata}, err
}

func (b shellBackend) Register(_ context.Context, req InstallRequest, _ CheckResult, id string) (Registration, error) {
	token := strings.Repeat("a", 48)
	spec, _, err := b.spec(req, token, id)
	if err != nil {
		return Registration{}, err
	}
	script, err := nodebootstrap.Build(spec)
	return Registration{Name: req.Name, Token: token, Script: script}, err
}

func (b shellBackend) Verify(ctx context.Context, _ string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(b.home, "started.pid")); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestOpenSSHUploadAndBootstrapRunEndToEndInIsolatedFixture(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("bootstrap requires a Unix platform")
	}
	home := t.TempDir()
	commands := filepath.Join(home, "commands")
	if err := os.Mkdir(commands, 0o700); err != nil {
		t.Fatal(err)
	}
	// This CLI fixture never contacts SSH: it executes exactly the final
	// remote command in an isolated HOME and PATH. OpenSSH's actual runner
	// still supplies the arguments and streams the original binary stdin.
	ssh := fmt.Sprintf(`#!/bin/sh
HOME='%s'
PATH='%s:/usr/bin:/bin'
SSH_CONNECTION='127.0.0.1 10001 127.0.0.1 22'
STEVE_SSH_TEST_PROCESS=1
export HOME PATH SSH_CONNECTION STEVE_SSH_TEST_PROCESS
for argument do final="$argument"; done
exec /bin/sh -c "$final"
`, home, commands)
	sshPath := filepath.Join(commands, "ssh-fixture")
	if err := os.WriteFile(sshPath, []byte(ssh), 0o700); err != nil {
		t.Fatal(err)
	}
	// Ignore login profiles in the fixture, while preserving the real binary
	// process and -config arguments produced by the bootstrap script.
	bash := "#!/bin/sh\nif [ \"$1\" = '-lc' ]; then exec /bin/sh -c \"$2\"; fi\nexec /bin/bash \"$@\"\n"
	if err := os.WriteFile(filepath.Join(commands, "bash"), []byte(bash), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config")
	if err := os.WriteFile(config, []byte("Host fixture\nHostName 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Options{ConfigPath: config, Runner: isolatedShellRunner{OpenSSH{Binary: sshPath}}, Backend: shellBackend{path: binary, home: home}})
	t.Cleanup(func() { _ = svc.Close() })
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(home, "started.pid")); err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil && pid > 0 {
				process, _ := os.FindProcess(pid)
				_ = process.Kill()
			}
		}
	})
	plan, err := svc.Plan(t.Context(), InstallRequest{Alias: "fixture", Name: "fixture-node", Addr: "127.0.0.1:7701"})
	if err != nil || !plan.Ready {
		t.Fatalf("isolated plan = %#v, %v", plan, err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil || !result.Connected {
		t.Fatalf("isolated commit = %#v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(home, "steve-bin", "node.json")); err != nil {
		t.Fatal("installation did not publish configuration")
	}
	if _, err := os.Stat(filepath.Join(home, "steve-bin", ".upload-"+plan.ID)); !os.IsNotExist(err) {
		t.Fatal("installation retained binary staging")
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err != nil {
		t.Fatal("idempotent outcome was not retained")
	}
}

//go:build linux

package peerstop

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func startArgumentPeer(t *testing.T, root string, args []string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, filepath.Join(root, "bin", "steve"), append([]string{"peer"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root)
	waitForFixturePeer(t, cmd)
	return cmd
}

func waitForFixturePeer(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
	}()
	select {
	case value := <-ready:
		if value != "ready" {
			t.Fatalf("fixture did not acquire its lock: %q", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not become ready")
	}
}

func TestPeerArgumentsRequireUniqueUnambiguousConfiguration(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"duplicate-config", []string{"--config", "config.json", "-config=config.json"}},
		{"duplicate-sidecar", []string{"--cluster-config=config.json.cluster.json", "-cluster-config", "config.json.cluster.json"}},
		{"missing-config", []string{"--config"}},
		{"empty-config", []string{"--config="}},
		{"missing-sidecar", []string{"--cluster-config"}},
		{"flag-as-value", []string{"--config", "--cluster-config"}},
		{"unknown-flag", []string{"--unexpected", "value"}},
		{"after-terminator", []string{"--", "--config", "config.json"}},
		{"positional", []string{"config.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, sidecar := installationFixture(t)
			peer := startArgumentPeer(t, root, tc.args)
			instance, err := loadInstallation(sidecar, "cluster", "node")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := instance.process(peer.Process.Pid); !errors.Is(err, ErrUnproven) {
				t.Fatalf("ambiguous installed peer was not refused: %v", err)
			}
			if err := instance.noPeer(t.Context()); !errors.Is(err, ErrUnproven) {
				t.Fatalf("absence scan discarded an ambiguous installed peer: %v", err)
			}
		})
	}
}

func TestPeerArgumentsRetainSupportedFlagSpellings(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--config", "config.json", "--cluster-config", "config.json.cluster.json"},
		{"-config=config.json", "-cluster-config=config.json.cluster.json"},
		{"--cluster-config=config.json.cluster.json", "-config", "config.json", "--"},
		{"--config=config.json", "--cluster-config="},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root, sidecar := installationFixture(t)
			peer := startArgumentPeer(t, root, args)
			instance, err := loadInstallation(sidecar, "cluster", "node")
			if err != nil {
				t.Fatal(err)
			}
			identity, found, err := instance.process(peer.Process.Pid)
			if err != nil || !found || identity.pid != peer.Process.Pid {
				t.Fatalf("valid peer arguments rejected: %+v %v %v", identity, found, err)
			}
		})
	}
}

func TestDuplicateSidecarOverrideCannotStopAnotherConfiguration(t *testing.T) {
	root, sidecar := installationFixture(t)
	other := filepath.Join(root, "other.cluster.json")
	if err := os.WriteFile(other, []byte(`{"cluster_id":"other-cluster","node_id":"other-node"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "bin", "steve"), "peer", "--config", filepath.Join(root, "config.json"), "--cluster-config", sidecar, "--cluster-config", other)
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root, "STEVE_PEERSTOP_PARSE_FLAGS=1")
	waitForFixturePeer(t, cmd)
	actual, err := os.ReadFile(filepath.Join(root, "effective-identity"))
	if err != nil || !strings.Contains(string(actual), "other-cluster") {
		t.Fatalf("fixture did not use the final sidecar: %q %v", actual, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := Stop(ctx, sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("override was accepted: %v", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("the other configuration's peer was signalled")
	}
}

func TestPeerIdentityReadFailureCannotAuthorizeAHandle(t *testing.T) {
	for _, failure := range []error{os.ErrPermission, unix.ESRCH, ErrUnproven} {
		reads, closes := 0, 0
		ops := processOps{
			identity: func(pid int) (processIdentity, bool, error) {
				reads++
				if reads == 2 {
					return processIdentity{}, false, failure
				}
				return processIdentity{pid: pid, start: 1}, true, nil
			},
			owner: func(*os.File) (int, error) { return 101, nil },
			open:  func(int) (int, error) { return 71, nil },
			close: func(int) error { closes++; return nil },
		}
		if fd, _, err := pinPeerIdentity(nil, 101, ops); fd != -1 || !errors.Is(err, ErrUnproven) || closes != 1 {
			t.Fatalf("unreadable identity left a signalable handle: fd=%d error=%v closes=%d", fd, err, closes)
		}
	}
}

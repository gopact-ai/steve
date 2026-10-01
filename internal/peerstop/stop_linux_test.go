//go:build linux

package peerstop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if os.Getenv("STEVE_PEERSTOP_FIXTURE") == "1" {
		root := os.Getenv("STEVE_PEERSTOP_ROOT")
		path := filepath.Join(root, "cluster", "peer-process", "gateway.lock")
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			os.Exit(2)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			os.Exit(3)
		}
		file.Truncate(0)
		fmt.Fprintln(file, os.Getpid())
		file.Sync()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("ready")
		for range signals {
			if os.Getenv("STEVE_PEERSTOP_IGNORE_TERM") != "1" {
				file.Close()
				os.Exit(0)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func installationFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"bin", "cluster/peer-process"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "bin", "steve")
	if err := os.Link(self, binary); err != nil {
		raw, readErr := os.ReadFile(self)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err = os.WriteFile(binary, raw, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sidecar := filepath.Join(root, "config.json.cluster.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "cluster_id": "cluster", "node_id": "node", "data_dir": filepath.Join(root, "cluster")})
	if err := os.WriteFile(sidecar, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cluster/peer-process/gateway.lock"), []byte("987654\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, sidecar
}

func startFixturePeer(t *testing.T, root string, ignore bool) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, "bin", "steve"), "peer", "--config", filepath.Join(root, "config.json"), "--cluster-config", filepath.Join(root, "config.json.cluster.json"))
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root, "STEVE_PEERSTOP_IGNORE_TERM="+map[bool]string{true: "1", false: "0"}[ignore])
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	ready := make(chan string, 1)
	go func() { raw := make([]byte, 64); n, _ := pipe.Read(raw); ready <- string(raw[:n]) }()
	select {
	case text := <-ready:
		if !strings.Contains(text, "ready") {
			t.Fatalf("peer not ready: %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not acquire lock")
	}
	return cmd, done
}

func TestStopRequiresExactInstallationBeforeSignalling(t *testing.T) {
	root, sidecar := installationFixture(t)
	cmd, _ := startFixturePeer(t, root, false)
	for _, pair := range [][2]string{{"other-cluster", "node"}, {"cluster", "other-node"}} {
		if _, err := Stop(t.Context(), sidecar, pair[0], pair[1]); !errors.Is(err, ErrUnproven) {
			t.Fatalf("wrong installation not classified: %v", err)
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatal("wrong installation killed the fixture")
		}
	}
}

func TestStopUsesTheLockOwnerAndWaitsForExit(t *testing.T) {
	root, sidecar := installationFixture(t)
	cmd, done := startFixturePeer(t, root, false)
	// The lock file's text is only a stale hint, not authority to signal it.
	if err := os.WriteFile(filepath.Join(root, "cluster/peer-process/gateway.lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	ran, err := Stop(ctx, sidecar, "cluster", "node")
	if err != nil || !ran {
		t.Fatalf("original installation did not stop: running=%v %v", ran, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("stop returned before peer exit")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("original process still exists")
	}
}

func TestStopUnknownOrMissingLockNeverClaimsAbsence(t *testing.T) {
	root, sidecar := installationFixture(t)
	if err := os.Remove(filepath.Join(root, "cluster/peer-process/gateway.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(t.Context(), sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("missing lock was accepted: %v", err)
	}
}

func TestStopAnEmptyIdentifiedInstallationDoesNotSignalStalePID(t *testing.T) {
	_, sidecar := installationFixture(t)
	ran, err := Stop(t.Context(), sidecar, "cluster", "node")
	if err != nil || ran {
		t.Fatalf("empty installation: %v %v", ran, err)
	}
}

func TestStopEscalatesThroughTheSameStableProcessHandle(t *testing.T) {
	root, sidecar := installationFixture(t)
	_, done := startFixturePeer(t, root, true)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ran, err := stopInstallationWithin(ctx, sidecar, "cluster", "node", 20*time.Millisecond)
	if err != nil || !ran {
		t.Fatalf("stable handle escalation: %v %v", ran, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("SIGKILL was not confirmed")
	}
}

func TestStopDeadlineDoesNotPretendTheOriginalProcessExited(t *testing.T) {
	root, sidecar := installationFixture(t)
	cmd, _ := startFixturePeer(t, root, true)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := Stop(ctx, sidecar, "cluster", "node"); !errors.Is(err, ErrRunning) {
		t.Fatalf("timed-out peer stop: %v", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("fixture should still ignore TERM")
	}
}

func TestStopRejectsAForeignExecutableHoldingTheInstallationLock(t *testing.T) {
	root, sidecar := installationFixture(t)
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-peer")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(foreign, raw, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(foreign, "peer", "--config", filepath.Join(root, "config.json"), "--cluster-config", sidecar)
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	ready := make(chan struct{})
	go func() { raw := make([]byte, 64); pipe.Read(raw); close(ready) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("foreign holder not ready")
	}
	if _, err := Stop(t.Context(), sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("foreign holder accepted: %v", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("foreign lock owner was signalled")
	}
}

func TestPinPeerRefusesReplacedIdentityOrLockBeforeAnySignal(t *testing.T) {
	for _, change := range []string{"exited", "reused", "lock-owner", "unsupported"} {
		t.Run(change, func(t *testing.T) {
			reads, closed := 0, 0
			ops := processOps{
				identity: func(pid int) (processIdentity, bool, error) {
					reads++
					if reads == 2 && change == "exited" {
						return processIdentity{}, false, nil
					}
					start := uint64(1)
					if reads == 2 && change == "reused" {
						start = 2
					}
					return processIdentity{pid, start}, true, nil
				},
				owner: func(*os.File) (int, error) {
					if change == "lock-owner" {
						return 202, nil
					}
					return 101, nil
				},
				open: func(int) (int, error) {
					if change == "unsupported" {
						return -1, unix.ENOSYS
					}
					return 71, nil
				},
				close: func(fd int) error {
					if fd != 71 {
						t.Error("closed another handle")
					}
					closed++
					return nil
				},
			}
			fd, err := pinPeer(nil, 101, ops)
			if fd != -1 || err == nil {
				t.Fatalf("replacement got a signalable handle: %d %v", fd, err)
			}
			if change != "unsupported" && closed != 1 {
				t.Fatal("unproved handle leaked")
			}
		})
	}
}

func TestAReapedPidfdCannotSignalANewInstallationInstance(t *testing.T) {
	root, sidecar := installationFixture(t)
	first, done := startFixturePeer(t, root, false)
	fd, err := unix.PidfdOpen(first.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := signalHandle(fd, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("old fixture did not exit")
	}
	replacement, _ := startFixturePeer(t, root, false)
	if err := signalHandle(fd, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("old stable handle signalled the replacement")
	}
	// The new request may stop the new, freshly identified instance.
	if _, err := Stop(t.Context(), sidecar, "cluster", "node"); err != nil {
		t.Fatal(err)
	}
}

func TestReplacedGatewayLockIsNotAccepted(t *testing.T) {
	root, _ := installationFixture(t)
	path := filepath.Join(root, "cluster/peer-process/gateway.lock")
	file, err := privateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("9999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sameLock(file); !errors.Is(err, ErrUnproven) {
		t.Fatalf("changed inode accepted: %v", err)
	}
}

//go:build linux

package peerstop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPeerRootMustMatchWithinTheSameNamespaces(t *testing.T) {
	if os.Getenv("STEVE_PEERSTOP_ROOT_TEST") != "1" {
		unshare := requirePrivateNamespace(t)
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, unshare, "--user", "--map-root-user", "--fork", "--kill-child", self, "-test.run=^TestPeerRootMustMatchWithinTheSameNamespaces$", "-test.v")
		cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_ROOT_TEST=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("root fixture: %v\n%s", err, out)
		}
		return
	}
	root, sidecar := installationFixture(t)
	privateRoot := filepath.Join(root, "private-root")
	if err := os.Mkdir(privateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "bin/steve"), "peer", "--config", filepath.Join(root, "config.json"), "--cluster-config", sidecar)
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root, "STEVE_PEERSTOP_CHROOT="+privateRoot)
	waitForFixturePeer(t, cmd)
	if err := checkPeerEnvironment(cmd.Process.Pid); !errors.Is(err, ErrUnproven) || !strings.Contains(err.Error(), "root") {
		t.Fatalf("different process root was not identified: %v", err)
	}
	if _, err := Stop(t.Context(), sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("different process root was not refused: %v", err)
	}
	assertFixtureStillHoldsItsLock(t, root)
}

func TestPeerNamespaceEvidenceMustBeReadable(t *testing.T) {
	if err := checkPeerEnvironment(os.Getpid()); err != nil {
		t.Fatalf("same process view rejected: %v", err)
	}
	if err := checkPeerEnvironment(-1); !errors.Is(err, ErrUnproven) {
		t.Fatalf("missing process view was accepted: %v", err)
	}
}

func requirePrivateNamespace(t *testing.T) string {
	t.Helper()
	program, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("private namespace fixture requires unshare")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, program, "--user", "--map-root-user", "--mount", "--fork", "true").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			t.Skip("private namespace fixture is not permitted on this host")
		}
		t.Fatalf("namespace fixture failed: %v %s", err, out)
	}
	return program
}

func TestPeerCannotBeStoppedThroughAnotherMountNamespacesConfiguration(t *testing.T) {
	unshare := requirePrivateNamespace(t)
	root, sidecar := installationFixture(t)
	other := filepath.Join(root, "other.cluster.json")
	if err := os.WriteFile(other, []byte(`{"version":1,"cluster_id":"other-cluster","node_id":"other-node","data_dir":"`+filepath.Join(root, "cluster")+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, unshare, "--user", "--map-root-user", "--mount", "--fork", "--kill-child", "bash", "-c", `set -e; mount --bind "$1" "$2"; exec "$3" peer --config "$4" --cluster-config "$2"`, "fixture", other, sidecar, filepath.Join(root, "bin/steve"), filepath.Join(root, "config.json"))
	cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root, "STEVE_PEERSTOP_PARSE_FLAGS=1")
	waitForFixturePeer(t, cmd)
	actual, err := os.ReadFile(filepath.Join(root, "effective-identity"))
	if err != nil || !strings.Contains(string(actual), "other-cluster") {
		t.Fatalf("fixture did not read its private configuration: %q %v", actual, err)
	}
	local, err := os.ReadFile(sidecar)
	if err != nil || strings.Contains(string(local), "other-cluster") {
		t.Fatalf("fixture changed the caller's installation: %q %v", local, err)
	}
	if _, err := Stop(ctx, sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
		t.Fatalf("another namespace's installation was accepted: %v", err)
	}
	assertFixtureStillHoldsItsLock(t, root)
}

func assertFixtureStillHoldsItsLock(t *testing.T, root string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(root, "cluster/peer-process/gateway.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		if err == nil {
			unix.Flock(int(file.Fd()), unix.LOCK_UN)
		}
		t.Fatalf("refused stop did not preserve the fixture's live lock: %v", err)
	}
}

func TestPeerInAnotherUserOrPIDNamespaceIsUnproven(t *testing.T) {
	unshare := requirePrivateNamespace(t)
	for _, pidNamespace := range []bool{false, true} {
		name := "user"
		if pidNamespace {
			name = "user-and-pid"
		}
		t.Run(name, func(t *testing.T) {
			root, sidecar := installationFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			args := []string{"--user", "--map-root-user", "--fork", "--kill-child"}
			if pidNamespace {
				args = append(args, "--pid")
			}
			args = append(args, filepath.Join(root, "bin/steve"), "peer", "--config", filepath.Join(root, "config.json"), "--cluster-config", sidecar)
			cmd := exec.CommandContext(ctx, unshare, args...)
			cmd.Env = append(os.Environ(), "STEVE_PEERSTOP_FIXTURE=1", "STEVE_PEERSTOP_ROOT="+root)
			waitForFixturePeer(t, cmd)
			if _, err := Stop(ctx, sidecar, "cluster", "node"); !errors.Is(err, ErrUnproven) {
				t.Fatalf("different process namespace was accepted: %v", err)
			}
			assertFixtureStillHoldsItsLock(t, root)
		})
	}
}

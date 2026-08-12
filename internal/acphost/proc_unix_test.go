//go:build unix

package acphost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKillProcessGroupReapsDescendants(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 60 & echo $! > %s; exec sleep 60", pidfile))
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessGroup(cmd)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(pidfile)
		if err == nil && len(raw) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child pidfile was not written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	childPID := 0
	raw, _ := os.ReadFile(pidfile)
	fmt.Sscanf(string(raw), "%d", &childPID)
	if childPID <= 0 {
		t.Fatal("bad child pid")
	}

	killProcessGroup(cmd)
	_ = cmd.Wait()

	if err := waitGone(childPID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

// waitGone polls until the pid no longer exists. A zombie (killed but not
// yet reaped by the system) also counts as gone.
func waitGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return nil
		}
		if stat, err := processState(pid); err == nil && strings.HasPrefix(stat, "Z") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d still alive after group kill", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processState(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

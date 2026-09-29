//go:build linux

package acphost

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leavingMember starts the agent the way a harness can: through a shell
// that leaves a member of the agent's process group running on its own. Its
// argument is unique to one test, so cleanup never signals a process the
// test did not start.
const leavingMember = `sleep "$3" </dev/null >/dev/null 2>&1 & echo $! > "$2/member"; exec "$1"`

// An agent that leaves in its own time, inside the grace a close gives it,
// may still leave members of its process group running. A stop is confirmed
// only once none of them is left.
func TestHostConfirmsStopOnlyAfterTheAgentsGroupIsGone(t *testing.T) {
	dir := t.TempDir()
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	t.Cleanup(func() {
		if pid := recordedPID(filepath.Join(dir, "member")); pid > 0 && cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	h := New(Config{Command: "/bin/sh", Args: []string{"-c", leavingMember, "agent", buildMockAgent(t), dir, pause}, ProcessDir: t.TempDir(), NoRestart: true})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	member := recordedPID(filepath.Join(dir, "member"))
	if member <= 0 || !liveProcess(member) {
		t.Fatal("the agent's shell left no member running")
	}
	// Closing its input is how a close asks the agent to leave; it leaves
	// at once, well inside the grace.
	h.Close()
	if !h.ProcessStopped(generation) {
		t.Fatal("the agent left but its stop was not confirmed")
	}
	if liveProcess(member) {
		t.Fatal("a stop was confirmed while a member of the agent's process group still ran")
	}
}

func recordedPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

func cmdlineIs(pid int, argv ...string) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Equal(raw, []byte(strings.Join(argv, "\x00")+"\x00"))
}

// liveProcess is false for a pid that is gone or a zombie: neither runs.
func liveProcess(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(raw, ')')
	fields := strings.Fields(string(raw[end+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

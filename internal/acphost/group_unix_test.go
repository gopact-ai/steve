//go:build linux || darwin

package acphost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
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

// A member the check for running members cannot see, as another user's
// process under a /proc mounted with hidepid, is still in the agent's
// process group after its leader is reaped. The stop is confirmed only once
// the kernel finds no process left in the group.
func TestHostDoesNotConfirmAStopWhileAHiddenMemberRuns(t *testing.T) {
	dir := t.TempDir()
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	endMember := func() {
		if pid := recordedPID(filepath.Join(dir, "member")); pid > 0 && cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	t.Cleanup(endMember)
	calls := kernelGroup
	calls.kill = func(int) error { return nil }
	calls.live = func(int) (bool, error) { return false, nil }
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", leavingMember, "agent", buildMockAgent(t), dir, pause}, ProcessDir: t.TempDir(),
		Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) }, group: &calls,
	}, NoRestart: true})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	member := recordedPID(filepath.Join(dir, "member"))
	if member <= 0 || !liveProcess(member) || leader.Load() <= 0 {
		t.Fatal("the agent's shell left no member running")
	}
	// The leader is this process's child, so its pid is not given out
	// again before it is reaped.
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !errors.Is(syscall.Kill(int(leader.Load()), 0), syscall.ESRCH); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the agent's leader was not reaped")
		}
	}
	for until := time.Now().Add(500 * time.Millisecond); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		if h.ProcessStopped(generation) {
			t.Fatal("a stop was confirmed while a member the check could not see still ran")
		}
	}
	endMember()
	for deadline := time.Now().Add(10 * time.Second); !h.ProcessStopped(generation); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop was not confirmed once the group was empty")
		}
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

// cmdlineIs reads the arguments pid runs with as ps shows them, which both
// Linux and macOS do alike.
func cmdlineIs(pid int, argv ...string) bool {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.TrimSpace(string(out)) == strings.Join(argv, " ")
}

// liveProcess is false for a pid that is gone or a zombie: neither runs.
func liveProcess(pid int) bool {
	state, err := processState(pid)
	return err == nil && state != "" && !strings.HasPrefix(state, "Z")
}

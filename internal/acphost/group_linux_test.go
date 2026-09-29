package acphost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gopact-ai/steve/internal/procgroup"
)

// adoptOrphans makes this process, for as long as the test runs, the
// reaper of the orphans its children leave, as a PID 1 is. It reaps none of
// them unless asked, as a PID 1 that reaps no orphans does not.
func adoptOrphans(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })
}

// reapOrphans kills, when the test ends, the orphans whose pids file names
// if they still run with the test's own argument, and reaps them, as they
// were left to this process.
func reapOrphans(t *testing.T, file, arg string) {
	t.Cleanup(func() {
		for _, pid := range recordedPIDs(file) {
			if cmdlineIs(pid, "sleep", arg) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			reap(pid)
		}
	})
}

// reap reaps the orphan pid left to this process once it has exited.
func reap(pid int) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if reaped, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); reaped == pid || err != nil {
			return
		}
	}
}

func recordedPIDs(file string) []int {
	raw, _ := os.ReadFile(file)
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// A PID 1 that reaps no orphans, as a container's often does not, leaves the
// members of an exited agent's process group zombies once they are killed.
// Nothing of the group runs, so its stop is confirmed, and the next agent
// starts as it would anyway.
func TestHostConfirmsAStopOnceOnlyZombiesAreLeftInTheAgentsGroup(t *testing.T) {
	adoptOrphans(t)
	dir := t.TempDir()
	members := filepath.Join(dir, "members")
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	reapOrphans(t, members, pause)
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `sleep "$3" </dev/null >/dev/null 2>&1 & echo $! >> "$2/members"; exec "$1"`, "agent", buildMockAgent(t), dir, pause},
		ProcessDir: t.TempDir(), Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) },
	}})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, first, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); !h.ProcessStopped(first); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the exited agent's stop was not confirmed while only zombies were left in its group")
		}
	}
	if left := recordedPIDs(members); len(left) == 0 {
		t.Fatal("the agent's shell left no member")
	} else if state, err := processState(left[0]); err != nil || !strings.HasPrefix(state, "Z") {
		t.Fatalf("the agent's member is %q (%v), not a zombie left in its group", state, err)
	}
	var second uint64
	for deadline := time.Now().Add(15 * time.Second); second <= first; time.Sleep(50 * time.Millisecond) {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, second, err = h.OpenSession(attempt, "", SessionConfig{Workdir: t.TempDir()})
		cancel()
		if time.Now().After(deadline) {
			t.Fatalf("no new agent started after the exited one stopped (generation %d, %v)", second, err)
		}
	}
}

// Where the listing of processes can miss one, as under /proc mounted with
// hidepid, zombies left in an exited agent's group do not show that nothing
// else is: its stop is confirmed only once the kernel finds no process in
// the group.
func TestHostDoesNotConfirmAStopOnZombiesWhereTheListingCanMissProcesses(t *testing.T) {
	adoptOrphans(t)
	dir := t.TempDir()
	member := filepath.Join(dir, "member")
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	reapOrphans(t, member, pause)
	calls := kernelGroup
	calls.inspect = func(group int) (procgroup.Remains, error) {
		remains, err := procgroup.Inspect(group)
		remains.Complete = false
		return remains, err
	}
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
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pid := recordedPID(member)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if state, err := processState(pid); err == nil && strings.HasPrefix(state, "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent's member was not left a zombie")
		}
	}
	for until := time.Now().Add(500 * time.Millisecond); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		if h.ProcessStopped(generation) {
			t.Fatal("a stop was confirmed on zombies where the listing could miss processes")
		}
	}
	reap(pid)
	for deadline := time.Now().Add(10 * time.Second); !h.ProcessStopped(generation); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop was not confirmed once the group was empty")
		}
	}
}

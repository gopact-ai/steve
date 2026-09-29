//go:build linux || darwin

package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	calls.inspect = func(group int) (procgroup.Remains, error) {
		remains, err := procgroup.Inspect(group)
		if !remains.Gone {
			remains.Running, remains.Complete = 0, false
		}
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

// An agent can exit leaving a member of its process group that no kill
// ends, as one in uninterruptible sleep. The host starts the next agent at
// once, while the stop of the one that exited stays unconfirmed until its
// group is empty: a new agent is no evidence the old one's group is gone.
func TestHostRestartsWhileAnExitedAgentsGroupCannotBeEnded(t *testing.T) {
	dir := t.TempDir()
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	endMembers := func() {
		raw, _ := os.ReadFile(filepath.Join(dir, "members"))
		for _, line := range strings.Fields(string(raw)) {
			if pid, _ := strconv.Atoi(line); pid > 0 && cmdlineIs(pid, "sleep", pause) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	calls := kernelGroup
	calls.kill = func(int) error { return nil }
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `sleep "$3" </dev/null >/dev/null 2>&1 & echo $! >> "$2/members"; exec "$1"`, "agent", buildMockAgent(t), dir, pause},
		ProcessDir: t.TempDir(), Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) }, group: &calls,
	}})
	t.Cleanup(h.Close)
	t.Cleanup(endMembers)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, first, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var second uint64
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, second, err = h.OpenSession(attempt, "", SessionConfig{Workdir: t.TempDir()})
		cancel()
		if err == nil && second > first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new agent started while the exited one's group could not be ended (generation %d, %v)", second, err)
		}
	}
	for until := time.Now().Add(500 * time.Millisecond); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		if h.ProcessStopped(first) {
			t.Fatal("the exited agent's stop was confirmed while a member of its group still ran")
		}
	}
	endMembers()
	for deadline := time.Now().Add(10 * time.Second); !h.ProcessStopped(first); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the exited agent's stop was not confirmed once its group was empty")
		}
	}
}

// Once its agent has exited, a host has no agent left to ask to leave or to
// kill: the transport kills what the agent left in its process group as the
// agent exits. An abort, stop or close then waits only briefly for that
// group to empty, and a group no kill ends leaves the stop unconfirmed until
// ProcessStopped finds the group empty.
func TestHostStopWaitsOnlyBrieflyForAnExitedAgentsGroup(t *testing.T) {
	dir := t.TempDir()
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	endMembers := func() {
		raw, _ := os.ReadFile(filepath.Join(dir, "members"))
		for _, line := range strings.Fields(string(raw)) {
			if pid, _ := strconv.Atoi(line); pid > 0 && cmdlineIs(pid, "sleep", pause) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	calls := kernelGroup
	calls.kill = func(int) error { return nil }
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `sleep "$3" </dev/null >/dev/null 2>&1 & echo $! >> "$2/members"; exec "$1"`, "agent", buildMockAgent(t), dir, pause},
		ProcessDir: t.TempDir(), Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) }, group: &calls,
	}})
	t.Cleanup(h.Close)
	t.Cleanup(endMembers)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.mu.Lock()
		alive := h.alive
		h.mu.Unlock()
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the host did not see its agent exit")
		}
	}
	for _, stop := range []struct {
		name string
		call func()
	}{{"abort", func() { h.Abort(generation) }}, {"stop", h.Stop}, {"close", h.Close}} {
		began := time.Now()
		stop.call()
		if took := time.Since(began); took > 4*time.Second {
			t.Fatalf("%s took %v once the agent had exited", stop.name, took.Round(time.Millisecond))
		}
		if h.ProcessStopped(generation) {
			t.Fatalf("%s confirmed the stop while a member of the exited agent's group still ran", stop.name)
		}
	}
	endMembers()
	for deadline := time.Now().Add(10 * time.Second); !h.ProcessStopped(generation); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the exited agent's stop was not confirmed once its group was empty")
		}
	}
}

// A member the agent left in its process group can keep the agent's output
// open after the agent has exited. The agent's exit, not the end of its
// output, ends the group: the host sees the agent go, and the stop is
// confirmed once the member is killed.
func TestHostEndsAnExitedAgentsGroupWhileAMemberHoldsItsOutput(t *testing.T) {
	dir := t.TempDir()
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	member := func() int { return recordedPID(filepath.Join(dir, "member")) }
	t.Cleanup(func() {
		if pid := member(); pid > 0 && cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `sleep "$3" </dev/null 2>/dev/null & echo $! > "$2/member"; exec "$1"`, "agent", buildMockAgent(t), dir, pause},
		ProcessDir: t.TempDir(), Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) },
	}, NoRestart: true})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !liveProcess(member()) {
		t.Fatal("the agent left no member in its group")
	}
	if err := syscall.Kill(int(leader.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !h.ProcessStopped(generation); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.mu.Lock()
			alive := h.alive
			h.mu.Unlock()
			t.Fatalf("the exited agent's stop was not confirmed while a member of its group held its output: the host takes the agent for running: %v; the member runs: %v", alive, liveProcess(member()))
		}
	}
	if liveProcess(member()) {
		t.Fatal("the stop was confirmed while the member ran")
	}
}

// A mark names one process group. An agent whose group is not recorded
// does not carry on a mark this process inherited, as it would then share
// it with the group that mark was recorded for.
func TestLocalTransportDoesNotPassOnAnInheritedMark(t *testing.T) {
	t.Setenv(procgroup.MarkVariable, "inherited")
	dir := t.TempDir()
	proc, err := LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `echo "${` + procgroup.MarkVariable + `-none}" > "$1"`, "agent", filepath.Join(dir, "mark")}, ProcessDir: dir,
	}.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Stdin().Close()
	_, _ = io.Copy(io.Discard, proc.Stdout())
	if err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mark"))
	if err != nil {
		t.Fatal(err)
	}
	if mark := strings.TrimSpace(string(raw)); mark != "none" {
		t.Fatalf("the agent carries the mark %q", mark)
	}
}

// An agent whose process group cannot be identified, where the host is to
// report it, is not served: no record could name the group, and nothing
// could confirm its stop once the process that started it is gone. Its
// group is ended, and it stays on the host's books until its stop is
// confirmed.
func TestHostEndsAnAgentWhoseProcessGroupCannotBeIdentified(t *testing.T) {
	failed := errors.New("process group cannot be identified")
	var leader atomic.Int64
	var held, reported atomic.Bool
	held.Store(true)
	calls := kernelGroup
	calls.capture = func(pid int, _ string) (procgroup.Identity, error) {
		leader.Store(int64(pid))
		return procgroup.Identity{}, failed
	}
	calls.inspect = func(group int) (procgroup.Remains, error) {
		if held.Load() {
			// As a member the kill cannot end yet.
			return procgroup.Remains{Running: 1}, nil
		}
		return procgroup.Inspect(group)
	}
	h := New(Config{Transport: LocalTransport{
		Command: buildMockAgent(t), ProcessDir: t.TempDir(),
		Started: func(procgroup.Identity) { reported.Store(true) }, group: &calls,
	}, NoRestart: true})
	t.Cleanup(h.Close)
	t.Cleanup(func() { held.Store(false) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, failed) {
		t.Fatalf("OpenSession = %v, want %v", err, failed)
	}
	if reported.Load() {
		t.Fatal("a process group that was not identified was reported")
	}
	pid := int(leader.Load())
	for deadline := time.Now().Add(10 * time.Second); liveProcess(pid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if liveProcess(pid) {
		t.Fatal("the agent whose process group was not identified still runs")
	}
	if h.AllProcessesStopped() {
		t.Fatal("the stop was confirmed while the agent's process group still had a member")
	}
	held.Store(false)
	for deadline := time.Now().Add(10 * time.Second); !h.AllProcessesStopped() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.AllProcessesStopped() {
		t.Fatal("the stop was not confirmed once the agent's process group was empty")
	}
}

// When the agent's exit cannot be watched without reaping it, waiting for
// the agent must still leave a close free to kill it: an agent that stays
// past the grace is killed, and the close returns.
func TestHostCloseKillsAnAgentWhoseExitCannotBeWatched(t *testing.T) {
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	var leader atomic.Int64
	t.Cleanup(func() {
		if pid := int(leader.Load()); pid > 0 && cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	calls := kernelGroup
	calls.waitExit = func(int) error { return errors.New("exit cannot be watched") }
	// The agent leaves its session when its input closes, and its process
	// then stays on without ever answering again.
	h := New(Config{Transport: LocalTransport{
		Command: "/bin/sh", Args: []string{"-c", `"$1"; exec sleep "$2"`, "agent", buildMockAgent(t), pause}, ProcessDir: t.TempDir(),
		Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) }, group: &calls,
	}, NoRestart: true})
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		h.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the close did not return while the agent's exit could not be watched")
	}
	if liveProcess(int(leader.Load())) {
		t.Fatal("the close returned while the agent still ran")
	}
	if h.ProcessStopped(generation) {
		t.Fatal("a stop was confirmed although the agent's process group was never checked")
	}
}

// An agent whose exit cannot be watched without reaping it is reaped only
// while no kill of its group is under way. A kill checks that the leader
// is not reaped and signals the group's id under one lock, and once the
// leader is reaped the id can be given to another process's group.
func TestLocalProcessReapsAnAgentWhoseExitCannotBeWatchedOnlyBetweenKills(t *testing.T) {
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	calls := kernelGroup
	calls.waitExit = func(int) error { return errors.New("exit cannot be watched") }
	proc, err := LocalTransport{Command: "sleep", Args: []string{pause}, ProcessDir: t.TempDir(), group: &calls}.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	local := proc.(*localProcess)
	pid := local.cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- proc.Wait() }()
	t.Cleanup(func() {
		if cmdlineIs(pid, "sleep", pause) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		<-waited
	})
	// As a kill does between its check and its signal.
	local.mu.Lock()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		local.mu.Unlock()
		t.Fatal(err)
	}
	reaped := false
	for deadline := time.Now().Add(time.Second); !reaped && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		reaped = errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	local.mu.Unlock()
	if reaped {
		t.Fatal("the agent was reaped while a kill of its group was under way")
	}
	select {
	case <-proc.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was not reaped once no kill was under way")
	}
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatal("Exited was closed before the agent was reaped")
	}
}

// An agent reaped without its exit being watched still tells its exit code.
func TestLocalProcessTellsTheExitCodeOfAnAgentWhoseExitCannotBeWatched(t *testing.T) {
	calls := kernelGroup
	calls.waitExit = func(int) error { return errors.New("exit cannot be watched") }
	proc, err := LocalTransport{Command: "/bin/sh", Args: []string{"-c", "exit 3"}, ProcessDir: t.TempDir(), group: &calls}.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var exit interface{ ExitCode() int }
	if err := proc.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("Wait = %v, want an exit code of 3", err)
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

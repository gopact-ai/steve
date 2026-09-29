package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

// Transport starts one agent process and hands back the pipes that talk to
// it. It is the only place the host knows *where* an agent runs: the local
// implementation forks a subprocess, and a remote one opens a stream to the
// node that forks it there.
//
// The whole ACP surface Steve uses is pure messages — it advertises only
// Elicitation as a client capability, so no method ever asks the client to
// touch a filesystem — which is why swapping these two pipes is enough to
// move an agent to another machine.
type Transport interface {
	// Start launches the agent. The returned Process is running; the caller
	// owns it until Wait returns.
	Start(ctx context.Context) (Process, error)
	// Name identifies the transport in logs and errors ("npx", "host-3/codex").
	Name() string
}

// Process is a running agent and its stdio. Stdout is what the agent writes;
// Stdin is what the host writes to it.
type Process interface {
	Stdout() io.ReadCloser
	Stdin() io.WriteCloser
	// Wait releases this transport's resources after its logical lifetime.
	// It is called exactly once. Remote stream loss alone does not prove
	// process exit; Stopped is the evidence.
	Wait() error
	// Exited is closed once the agent's own process has exited. What it
	// left running may outlive it: Wait returns only once that is settled
	// too.
	Exited() <-chan struct{}
	// Kill forces the agent and everything it spawned to die — the escape
	// hatch for a graceful close that did not settle.
	Kill()
	// Stopped reports positive evidence that the agent process is gone: a
	// reaped child whose process group the kernel finds no process left in,
	// or a node's confirmed exit. A transport that cannot tell answers false,
	// and the host keeps the process on its books.
	Stopped() bool
}

// LocalTransport forks the agent as a child of this process, leading a
// process group of its own.
type LocalTransport struct {
	Command    string
	Args       []string
	ProcessDir string
	Env        []string
	// Started, when set, learns the identity of each process group this
	// transport starts as soon as its leader runs. The leader's environment
	// then carries the group's mark, and what it spawns inherits it; without
	// it the leader carries no mark.
	Started func(procgroup.Identity)
	// group makes the calls on the agent's process group; nil makes them
	// on the kernel.
	group *groupCalls
}

// groupCalls are the calls a local process makes on its process group, so a
// test can stand in for a kernel that hides a member or cannot end one.
type groupCalls struct {
	waitExit func(pid int) error
	kill     func(group int) error
	live     func(group int) (bool, error)
	gone     func(group int) (bool, error)
}

var kernelGroup = groupCalls{waitExit: procgroup.WaitExit, kill: procgroup.Kill, live: procgroup.Live, gone: procgroup.Gone}

func (t LocalTransport) Name() string { return t.Command }

func (t LocalTransport) Start(context.Context) (Process, error) {
	processDir := t.ProcessDir
	if processDir == "" {
		processDir = "."
	}
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent workdir: %w", err)
	}
	cmd := exec.Command(t.Command, t.Args...)
	setProcessGroup(cmd)
	cmd.Dir = processDir
	cmd.Env = mergeEnv(os.Environ(), t.Env)
	mark := ""
	if t.Started != nil {
		mark = procgroup.NewMark()
		cmd.Env = mergeEnv(cmd.Env, []string{procgroup.MarkVariable + "=" + mark})
	} else {
		// A mark names one recorded group; one this process inherited
		// would make the agent's group pass for that one.
		cmd.Env = withoutEnv(cmd.Env, procgroup.MarkVariable)
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent %q: %w", t.Command, err)
	}
	if t.Started != nil {
		// The leader is not waited for before Start returns, so it still
		// holds its pid and its start time can be read.
		if id, err := procgroup.Capture(cmd.Process.Pid, mark); err != nil {
			slog.Warn(fmt.Sprintf("acphost: agent process group not identified: %v", err))
		} else {
			t.Started(id)
		}
	}
	group := kernelGroup
	if t.group != nil {
		group = *t.group
	}
	return &localProcess{cmd: cmd, stdout: stdout, stdin: stdin, group: group, exited: make(chan struct{})}, nil
}

type localProcess struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stdin   io.WriteCloser
	group   groupCalls
	exited  chan struct{}
	stopped atomic.Bool
	// mu keeps a kill from reaching the group's id once the leader is
	// reaped: from then on the id can belong to another process's group.
	mu     sync.Mutex
	reaped bool
}

func (p *localProcess) Stdout() io.ReadCloser   { return p.stdout }
func (p *localProcess) Stdin() io.WriteCloser   { return p.stdin }
func (p *localProcess) Exited() <-chan struct{} { return p.exited }

// Wait kills what the agent left running in its process group before
// reaping the agent. Until the leader is reaped it holds its pid, so no
// other process can lead a group of that id, and the kill reaches only
// the agent's own. The stop is confirmed once the kernel finds no process
// left in the group. Exited is closed as soon as the agent has exited, as
// a member no kill ends can keep Wait waiting for as long as it runs.
func (p *localProcess) Wait() error {
	pid := p.cmd.Process.Pid
	switch err := p.group.waitExit(pid); {
	case errors.Is(err, procgroup.ErrUnsupported):
		err := p.reapRunning()
		p.stopped.Store(true)
		return err
	case err != nil:
		// Nothing then shows the group is empty, so the stop stays
		// unconfirmed.
		slog.Error(fmt.Sprintf("acphost: wait for agent process %d: %v", pid, err))
		return p.reapRunning()
	}
	close(p.exited)
	p.endGroup(pid)
	p.mu.Lock()
	err := p.cmd.Wait()
	p.reaped = true
	p.mu.Unlock()
	p.awaitEmpty(pid)
	p.stopped.Store(true)
	return err
}

// reapRunning reaps an agent whose exit cannot be watched without reaping
// it. The agent may still run, so the lock stays free and a kill can reach
// it; a kill that comes just as the agent is reaped can then reach the
// group's id after it is let go.
func (p *localProcess) reapRunning() error {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	close(p.exited)
	return err
}

// endGroup kills the members of an exited leader's group until none that
// this process can see runs. A member the kill cannot end, as one in
// uninterruptible sleep until it wakes, keeps it waiting.
func (p *localProcess) endGroup(group int) {
	stuck := newStuckReport()
	for delay := time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		killErr := p.group.kill(group)
		live, err := p.group.live(group)
		if err == nil && !live {
			return
		}
		if waited, due := stuck.due(); due {
			slog.Error(fmt.Sprintf("acphost: agent process group %d still runs %s after SIGKILL (kill: %v, check: %v)", group, waited, killErr, err))
		}
		time.Sleep(delay)
	}
}

// awaitEmpty waits, the leader reaped, until the kernel finds no process
// left in its group: a member hidden from this process, or one it may not
// signal, is not listed as running but still holds the group's id. Nothing
// is signalled any more, as the id stops being the agent's own once its
// last member is gone.
func (p *localProcess) awaitEmpty(group int) {
	stuck := newStuckReport()
	for delay := time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		gone, err := p.group.gone(group)
		if err == nil && gone {
			return
		}
		if waited, due := stuck.due(); due {
			slog.Error(fmt.Sprintf("acphost: agent process group %d still has a process this one cannot end after %s (check: %v)", group, waited, err))
		}
		time.Sleep(delay)
	}
}

// stuckReport paces the report of a group that does not settle: first
// after five seconds, then once a minute for as long as it stays.
type stuckReport struct{ began, next time.Time }

func newStuckReport() *stuckReport {
	now := time.Now()
	return &stuckReport{began: now, next: now.Add(5 * time.Second)}
}

// due reports whether the group has waited long enough to be reported
// again, and how long it has waited.
func (r *stuckReport) due() (time.Duration, bool) {
	now := time.Now()
	if now.Before(r.next) {
		return 0, false
	}
	r.next = now.Add(time.Minute)
	return now.Sub(r.began).Round(time.Second), true
}

func (p *localProcess) Stopped() bool { return p.stopped.Load() }

// Kill signals the whole process group: the agent spawns MCP stdio servers
// and tools of its own, and killing only the parent orphans them.
func (p *localProcess) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaped {
		killProcessGroup(p.cmd)
	}
}

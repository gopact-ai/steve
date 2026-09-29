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
	capture  func(pid int, mark string) (procgroup.Identity, error)
	waitExit func(pid int) error
	kill     func(group int) error
	inspect  func(group int) (procgroup.Remains, error)
}

var kernelGroup = groupCalls{capture: procgroup.Capture, waitExit: procgroup.WaitExit, kill: procgroup.Kill, inspect: procgroup.Inspect}

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
	group := kernelGroup
	if t.group != nil {
		group = *t.group
	}
	if t.Started != nil {
		// The leader is not waited for before Start returns, so it still
		// holds its pid and its start time can be read.
		if id, err := group.capture(cmd.Process.Pid, mark); err != nil {
			slog.Warn(fmt.Sprintf("acphost: agent process group not identified: %v", err))
		} else {
			t.Started(id)
		}
	}
	p := &localProcess{cmd: cmd, stdout: stdout, stdin: stdin, group: group, exited: make(chan struct{}), observed: make(chan struct{})}
	go p.observe()
	return p, nil
}

type localProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stdin  io.WriteCloser
	group  groupCalls
	exited chan struct{}
	// observed is closed once observe is over, err then holding how the
	// agent ended.
	observed chan struct{}
	err      error
	stopped  atomic.Bool
	// mu keeps a kill from reaching the group's id once the leader is
	// reaped: from then on the id can belong to another process's group.
	mu     sync.Mutex
	reaped bool
}

func (p *localProcess) Stdout() io.ReadCloser   { return p.stdout }
func (p *localProcess) Stdin() io.WriteCloser   { return p.stdin }
func (p *localProcess) Exited() <-chan struct{} { return p.exited }

// observe watches the agent from its start, apart from its output: a member
// of its process group can hold that output open after the agent has
// exited, and only the end of the group closes it then. It kills what the
// agent left running in its group before reaping the agent. Until the
// leader is reaped it holds its pid, so no other process can lead a group
// of that id, and the kill reaches only the agent's own. The stop is
// confirmed once nothing of the group runs, as procgroup.Remains.Ended
// judges. Exited is closed as soon as the agent has exited, as a member no
// kill ends can keep observe waiting for as long as it runs. Nothing cuts
// that wait short: a host's Close waits for it only a bounded time, so
// observe, and the checks in endGroup and awaitEmpty, can run past the
// Close until the group is empty.
func (p *localProcess) observe() {
	defer close(p.observed)
	pid := p.cmd.Process.Pid
	switch err := p.group.waitExit(pid); {
	case errors.Is(err, procgroup.ErrUnsupported):
		p.err = p.reapRunning()
		p.stopped.Store(true)
		return
	case err != nil:
		// Nothing then shows the group is empty, so the stop stays
		// unconfirmed.
		slog.Error(fmt.Sprintf("acphost: wait for agent process %d: %v", pid, err))
		p.err = p.reapRunning()
		return
	}
	close(p.exited)
	p.endGroup(pid)
	p.mu.Lock()
	p.err = exitError(p.cmd.Process.Wait())
	p.reaped = true
	p.mu.Unlock()
	p.awaitEmpty(pid)
	p.stopped.Store(true)
}

// Wait returns once observe is over, and lets go of the agent's pipes: it
// is called once what the agent wrote has been read, as the agent is
// reaped without them.
func (p *localProcess) Wait() error {
	<-p.observed
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	return p.err
}

// exitError is what exec.Cmd.Wait says of how the agent ended.
func exitError(state *os.ProcessState, err error) error {
	if err == nil && !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return err
}

// endGroup kills the members of an exited leader's group until none that
// this process can list runs. A member the kill cannot end, as one in
// uninterruptible sleep until it wakes, keeps it waiting.
func (p *localProcess) endGroup(group int) {
	stuck := newStuckReport()
	for delay := time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		killErr := p.group.kill(group)
		remains, err := p.group.inspect(group)
		if err == nil && remains.Running == 0 {
			return
		}
		if waited, due := stuck.due(); due {
			slog.Error(fmt.Sprintf("acphost: agent process group %d still runs %s after SIGKILL: %s (kill: %v)", group, waited, left(remains, err), killErr))
		}
		time.Sleep(delay)
	}
}

// awaitEmpty waits, the leader reaped, until nothing of its group runs: a
// member hidden from this process is not listed as running but still keeps
// the stop unconfirmed. Nothing is signalled any more, as the id stops
// being the agent's own once its last member is gone.
func (p *localProcess) awaitEmpty(group int) {
	stuck := newStuckReport()
	for delay := time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		remains, err := p.group.inspect(group)
		if err == nil && remains.Ended() {
			return
		}
		if waited, due := stuck.due(); due {
			slog.Error(fmt.Sprintf("acphost: agent process group %d has not stopped %s after its leader was reaped: %s", group, waited, left(remains, err)))
		}
		time.Sleep(delay)
	}
}

// left says what a check found left in a group, for a report of one that
// does not end.
func left(remains procgroup.Remains, err error) string {
	if err != nil {
		return fmt.Sprintf("the check failed: %v", err)
	}
	return remains.String()
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

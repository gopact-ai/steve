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
	// Kill forces the agent and everything it spawned to die — the escape
	// hatch for a graceful close that did not settle.
	Kill()
	// Stopped reports positive evidence that the agent process is gone: a
	// reaped child whose process group has no running member left, or a
	// node's confirmed exit. A transport that cannot tell answers false,
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
	// then carries the group's mark, and what it spawns inherits it.
	Started func(procgroup.Identity)
}

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
	return &localProcess{cmd: cmd, stdout: stdout, stdin: stdin}, nil
}

type localProcess struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stdin   io.WriteCloser
	stopped atomic.Bool
	// mu keeps a kill from reaching the group's id once the leader is
	// reaped: from then on the id can belong to another process's group.
	mu     sync.Mutex
	reaped bool
}

func (p *localProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }

// Wait kills what the agent left running in its process group before
// reaping the agent. Until the leader is reaped it holds its pid, so no
// other process can lead a group of that id, and the kill reaches only
// the agent's own. The stop is confirmed once no member runs.
func (p *localProcess) Wait() error {
	pid := p.cmd.Process.Pid
	ended := false
	switch err := procgroup.WaitExit(pid); {
	case errors.Is(err, procgroup.ErrUnsupported):
		err := p.cmd.Wait()
		p.mu.Lock()
		p.reaped = true
		p.mu.Unlock()
		p.stopped.Store(true)
		return err
	case err != nil:
		slog.Error(fmt.Sprintf("acphost: wait for agent process %d: %v", pid, err))
	default:
		ended = endGroup(pid)
	}
	p.mu.Lock()
	err := p.cmd.Wait()
	p.reaped = true
	p.mu.Unlock()
	if ended {
		p.stopped.Store(true)
	}
	return err
}

// endGroup kills the members of an exited leader's group until none runs.
// A member the kill cannot end, as one in uninterruptible sleep until it
// wakes, keeps it waiting and the stop unconfirmed.
func endGroup(group int) bool {
	began, warned := time.Now(), false
	for delay := time.Millisecond; ; delay = min(2*delay, time.Second) {
		killErr := procgroup.Kill(group)
		live, err := procgroup.Live(group)
		if err == nil && !live {
			return true
		}
		if !warned && time.Since(began) > 5*time.Second {
			warned = true
			slog.Error(fmt.Sprintf("acphost: agent process group %d still runs after SIGKILL (kill: %v, check: %v)", group, killErr, err))
		}
		time.Sleep(delay)
	}
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

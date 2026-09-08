package acphost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
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
	// reaped child, or a node's confirmed exit. A transport that cannot
	// tell answers false, and the host keeps the process on its books.
	Stopped() bool
}

// LocalTransport forks the agent as a child of this process.
type LocalTransport struct {
	Command    string
	Args       []string
	ProcessDir string
	Env        []string
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
	return &localProcess{cmd: cmd, stdout: stdout, stdin: stdin}, nil
}

type localProcess struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stdin   io.WriteCloser
	stopped atomic.Bool
}

func (p *localProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Wait() error {
	err := p.cmd.Wait()
	p.stopped.Store(true)
	return err
}

func (p *localProcess) Stopped() bool { return p.stopped.Load() }

// Kill signals the whole process group: the agent spawns MCP stdio servers
// and tools of its own, and killing only the parent orphans them.
func (p *localProcess) Kill() { killProcessGroup(p.cmd) }

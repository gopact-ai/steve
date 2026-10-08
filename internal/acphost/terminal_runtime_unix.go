//go:build linux || darwin

package acphost

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/procgroup"
	"golang.org/x/sys/unix"
)

func openTerminalDirectory(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
}

func localTerminalAvailable(proc Process) bool {
	p, ok := proc.(*localProcess)
	if !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminalAdmissionLocked() == nil
}
func validateLocalTerminal(proc Process) error {
	p, ok := proc.(*localProcess)
	if !ok {
		return procgroup.ErrUnsupported
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminalAdmissionLocked()
}
func localTerminalConfig(proc Process, req *acp.CreateTerminalRequest) (terminalChildConfig, error) {
	p, ok := proc.(*localProcess)
	if !ok {
		return terminalChildConfig{}, procgroup.ErrUnsupported
	}
	p.mu.Lock()
	if err := p.terminalAdmissionLocked(); err != nil {
		p.mu.Unlock()
		return terminalChildConfig{}, err
	}
	// Profile was frozen by the original agent transport. User overrides never
	// affect the helper command, its launch environment, or the protected mark.
	env := append([]string(nil), p.cmd.Env...)
	originalGroup, originalMark := p.identity.Group, p.identity.Mark
	p.mu.Unlock()
	overrides := make([]string, len(req.Env))
	for i, item := range req.Env {
		overrides[i] = item.Name + "=" + item.Value
	}
	env = withoutEnv(mergeEnv(env, overrides), procgroup.MarkVariable)
	return terminalChildConfig{Command: req.Command, Args: append([]string(nil), req.Args...), Env: env, ParentGroup: originalGroup, ParentMark: originalMark, Mark: procgroup.NewMark()}, nil
}

// ownedTerminalProcess adopts the trusted helper before SPLIT. Its observer
// is the only cmd.Wait caller; until group proof it retains the leader PID.
type ownedTerminalProcess struct {
	helper        *terminalHelper
	group         groupCalls
	done          chan struct{}
	nativeStopped atomic.Bool
	mu            sync.Mutex
	status        *acp.TerminalExitStatus
	outputReader  *os.File
	drained       <-chan error
}

func prepareLocalTerminal(ctx context.Context, original Process, cwd *os.File, config terminalChildConfig, output *terminalOutput, helperArgs []string) (terminalRuntime, error) {
	parent, ok := original.(*localProcess)
	if !ok {
		return nil, procgroup.ErrUnsupported
	}
	if len(helperArgs) == 0 {
		helperArgs = []string{TerminalChildVerb}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	drained := make(chan error, 1)
	go func() { _, err := io.CopyBuffer(output, reader, make([]byte, 32<<10)); drained <- err }()
	helper, err := parent.prepareTerminal(ctx, cwd, config, writer, helperArgs)
	writer.Close()
	if err != nil {
		// Only the inert child existed, and prepareTerminal reaps its failures.
		reader.Close()
		<-drained
		return nil, err
	}
	runtime := &ownedTerminalProcess{helper: helper, group: kernelGroup, done: make(chan struct{}), outputReader: reader, drained: drained}
	parent.mu.Lock()
	err = parent.terminalAdmissionLocked()
	if parent.terminalProcesses == nil {
		parent.terminalProcesses = map[terminalRuntime]struct{}{}
	}
	// Even closing generations retain a newly started inert helper until its
	// sole observer supplies positive proof. No SPLIT can precede registration.
	parent.terminalProcesses[runtime] = struct{}{}
	parent.mu.Unlock()
	go runtime.observe()
	if err != nil {
		_ = runtime.signal()
		return runtime, err
	}
	return runtime, nil
}
func (p *ownedTerminalProcess) Preparation() procgroup.Preparation { return p.helper.preparation }
func (p *ownedTerminalProcess) Mark() string                       { return p.helper.mark }
func (p *ownedTerminalProcess) Place() procgroup.Place             { return p.helper.place }
func (p *ownedTerminalProcess) Split(ctx context.Context) (procgroup.Identity, error) {
	return p.helper.split(ctx)
}
func (p *ownedTerminalProcess) Execute(ctx context.Context) error { return p.helper.exec(ctx) }
func (p *ownedTerminalProcess) PayloadAdmitted() bool {
	p.helper.mu.Lock()
	defer p.helper.mu.Unlock()
	return p.helper.gateWritten
}
func (p *ownedTerminalProcess) Done() <-chan struct{} { return p.done }
func (p *ownedTerminalProcess) NativeStopped() bool   { return p.nativeStopped.Load() }
func (p *ownedTerminalProcess) ExitStatus() *acp.TerminalExitStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == nil {
		return nil
	}
	status := *p.status
	if status.ExitCode != nil {
		code := *status.ExitCode
		status.ExitCode = &code
	}
	if status.Signal != nil {
		signal := *status.Signal
		status.Signal = &signal
	}
	return &status
}

func (p *ownedTerminalProcess) signal() error {
	h := p.helper
	h.mu.Lock()
	defer h.mu.Unlock()
	h.control.Close()
	if p.nativeStopped.Load() {
		return nil
	}
	if !h.waited {
		// The direct child pins its PID until the sole observer reaps it. Before
		// any EXEC gate the trusted inert program has forked no descendants.
		if h.payloadStarted && h.identity.Group != 0 {
			return p.group.kill(h.identity.Group)
		}
		return h.cmd.Process.Kill()
	}
	// An observer never reaps an executed leader before group proof. Its
	// exceptional wait failure is not permission to signal a stale numeric ID.
	return nil // Reaping began only after proof; await the sole observer.
}
func (p *ownedTerminalProcess) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.nativeStopped.Load() {
		return nil
	}
	if err := p.signal(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
	}
	if !p.nativeStopped.Load() {
		return procgroup.ErrUnproven
	}
	return nil
}

func (p *ownedTerminalProcess) observe() {
	defer close(p.done)
	defer p.outputReader.Close()
	h := p.helper
	err := p.group.waitExit(h.cmd.Process.Pid)
	h.mu.Lock()
	h.control.Close()
	executed, id := h.payloadStarted, h.identity
	// Keep this lock until proof and reaping are sequenced. Split and EXEC have
	// finite pipe deadlines; no Node owner callback is made under this lock.
	proved := err == nil
	if executed {
		proved = false
		if err == nil && id.Group != 0 {
			// The unreaped leader pins the group, so descendant signals cannot reach
			// a reused PGID. Hidden or stubborn writers keep proof false and the
			// observer alive; caller cleanup deadlines do not fake stop evidence.
			for delay := time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
				_ = p.group.kill(id.Group)
				remains, inspectErr := p.group.inspect(id.Group)
				if inspectErr == nil && remains.Ended() {
					proved = true
					break
				}
				h.mu.Unlock()
				time.Sleep(delay)
				h.mu.Lock()
			}
		}
	}
	h.status.Close()
	h.waited, h.ended = true, true // No signal may race cmd.Wait PID release.
	h.mu.Unlock()
	// cmd.Wait is exactly once, after the leader has stopped and all admitted
	// group writers are quiet. The explicit pipe drain below is also required;
	// its deadline is not permission to claim quiet for an unknown writer.
	waitErr := h.cmd.Wait()
	h.mu.Lock()
	h.waited, h.ended = true, true
	h.mu.Unlock()
	state := h.cmd.ProcessState
	if state != nil {
		status := &acp.TerminalExitStatus{}
		if raw, ok := state.Sys().(syscall.WaitStatus); ok && raw.Signaled() {
			signal := unix.SignalName(raw.Signal())
			status.Signal = &signal
		} else if state.ExitCode() >= 0 {
			code := uint32(state.ExitCode())
			status.ExitCode = &code
		}
		p.mu.Lock()
		p.status = status
		p.mu.Unlock()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		proved = false
	}
	drainTimer := time.NewTimer(ExitedOutputWait)
	select {
	case drainErr := <-p.drained:
		if drainErr != nil {
			proved = false
		}
	case <-drainTimer.C:
		proved = false // An inherited writer did not quiesce with the owned group.
		p.outputReader.Close()
		<-p.drained
	}
	drainTimer.Stop()
	if proved {
		p.nativeStopped.Store(true)
		h.parent.mu.Lock()
		delete(h.parent.terminalProcesses, p)
		h.parent.mu.Unlock()
	}
}

//go:build linux || darwin

package acphost

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

// terminalHelper is inert until the owner commits its preparation and group
// identities and consumes the payload gate. Its cmd has exactly one waiter.
type terminalHelper struct {
	parent         *localProcess
	cmd            *exec.Cmd
	control        *os.File
	status         *os.File
	reader         *bufio.Reader
	preparation    procgroup.Preparation
	identity       procgroup.Identity
	place          procgroup.Place
	mark           string
	mu             sync.Mutex
	ended          bool
	waited         bool
	payloadStarted bool
	gateWritten    bool
}

func (p *localProcess) closeTerminalAdmission() {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
}

func (p *localProcess) terminalAdmissionLocked() error {
	if p.closing || p.reaped || p.unidentified != nil || p.identity.Group == 0 || p.place.Boot == "" {
		return errors.New("original native process cannot admit a terminal")
	}
	select {
	case <-p.exited:
		return errors.New("original native process has exited")
	default:
		return nil
	}
}

// prepareTerminal starts only the trusted inert helper, initially in the
// original Agent group. The caller already owns its original callback/root.
func (p *localProcess) prepareTerminal(ctx context.Context, cwd *os.File, config terminalChildConfig,
	output io.Writer, helperArgs []string) (*terminalHelper, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	control, gate, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	status, notice, err := os.Pipe()
	if err != nil {
		control.Close()
		gate.Close()
		return nil, err
	}
	self, err := os.Open("/proc/self/exe")
	path := "/proc/self/fd/6"
	if runtime.GOOS == "darwin" {
		var executable string
		executable, err = os.Executable()
		if err == nil {
			self, err = os.Open(executable)
		}
		path = "/dev/fd/6"
	}
	if err != nil {
		control.Close()
		gate.Close()
		status.Close()
		notice.Close()
		return nil, err
	}
	defer self.Close()
	cmd := exec.Command(path, helperArgs...)
	cmd.ExtraFiles = []*os.File{control, cwd, notice, self}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Stdin = nil
	cmd.WaitDelay = 250 * time.Millisecond
	p.mu.Lock()
	err = p.terminalAdmissionLocked()
	if err == nil {
		config.ParentGroup, config.ParentMark = p.identity.Group, p.identity.Mark
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: p.identity.Group}
		cmd.Env = mergeEnv(p.cmd.Env, []string{procgroup.MarkVariable + "=" + p.identity.Mark})
		err = cmd.Start()
	}
	p.mu.Unlock()
	control.Close()
	notice.Close()
	if err != nil {
		gate.Close()
		status.Close()
		return nil, err
	}
	helper := &terminalHelper{
		parent: p, cmd: cmd, control: gate, status: status, reader: bufio.NewReaderSize(status, 4096),
		place: p.place, mark: config.Mark,
	}
	if err := helper.deadline(ctx); err != nil {
		helper.endInert()
		return nil, err
	}
	if err := writeTerminalChildGate(gate, config); err != nil {
		helper.endInert()
		return nil, err
	}
	if err := terminalNotice(helper.reader, "preparing", cmd.Process.Pid); err != nil {
		helper.endInert()
		return nil, err
	}
	p.mu.Lock()
	err = p.terminalAdmissionLocked()
	if err == nil {
		helper.preparation, err = procgroup.CapturePreparation(cmd.Process.Pid, p.identity)
	}
	p.mu.Unlock()
	if err != nil {
		helper.endInert()
		return nil, err
	}
	return helper, nil
}

func (h *terminalHelper) deadline(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	return errors.Join(h.control.SetWriteDeadline(deadline), h.status.SetReadDeadline(deadline))
}

// split requires the exact preparation to have been durably retained by its
// owner before it is called. The return is a real leader-only Capture result.
func (h *terminalHelper) split(ctx context.Context) (procgroup.Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended || h.identity.Group != 0 {
		return procgroup.Identity{}, errors.New("terminal helper split is not available")
	}
	if err := ctx.Err(); err != nil {
		return procgroup.Identity{}, err
	}
	if err := h.deadline(ctx); err != nil {
		return procgroup.Identity{}, err
	}
	h.parent.mu.Lock()
	err := h.parent.terminalAdmissionLocked()
	if err == nil {
		_, err = io.WriteString(h.control, "SPLIT\n")
	}
	h.parent.mu.Unlock()
	if err != nil {
		return procgroup.Identity{}, err
	}
	if err := terminalNotice(h.reader, "active", h.cmd.Process.Pid); err != nil {
		return procgroup.Identity{}, err
	}
	h.identity, err = h.preparation.CapturedGroup(h.mark)
	return h.identity, err
}

// exec is consumed once, inside the owner's fresh execution-admission path.
// It does not make a returned create-RPC context the process's lifetime.
func (h *terminalHelper) exec(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended || h.identity.Group == 0 {
		return errors.New("terminal helper payload is not available")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.deadline(ctx); err != nil {
		return err
	}
	h.parent.mu.Lock()
	err := h.parent.terminalAdmissionLocked()
	if err == nil {
		h.ended, h.payloadStarted = true, true // Once-only, including an uncertain partial pipe write.
		_, err = io.WriteString(h.control, "EXEC\n")
	}
	h.parent.mu.Unlock()
	if err != nil {
		return err
	}
	h.gateWritten = true
	if err := h.control.Close(); err != nil {
		return err
	}
	// A failed lookup/exec has an explicit notice. A CLOEXEC EOF after the
	// trusted helper's executing notice accepts a terminal handle; it is not
	// used as payload execution or group-stop evidence.
	if err := terminalNotice(h.reader, "executing", h.cmd.Process.Pid); err != nil {
		return err
	}
	_, err = h.reader.ReadSlice('\n')
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("terminal payload launch was not acknowledged")
}

// endInert never releases an unproven numeric process-group identity. Before
// payload exec the helper forks nothing; its unreaped direct child is pinned.
func (h *terminalHelper) endInert() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.control.Close()
	h.status.Close()
	if h.waited {
		return nil
	}
	if h.ended {
		return errors.New("terminal payload cleanup requires its original group owner")
	}
	_ = h.cmd.Process.Kill()
	err := h.cmd.Wait()
	h.waited, h.ended = true, true
	return err
}

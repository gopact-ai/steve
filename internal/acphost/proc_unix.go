//go:build unix

package acphost

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// setProcessGroup puts the agent subprocess in its own process group so a
// forced shutdown can reap its descendants (MCP stdio servers, spawned
// tools) instead of orphaning them.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the agent's process group.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Killing our own child's group can only fail with ESRCH, the group
	// already gone, which is the state a kill wants.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// reapRunning reaps an agent whose exit cannot be watched without reaping
// it. It checks for the exit under the lock a kill holds from its check to
// its signal, so the leader is never reaped, and the group's id let go,
// while a kill is under way. A reap that blocked could not take that lock,
// as the kill that ends the agent may need it first.
func (p *localProcess) reapRunning() error {
	pid := p.cmd.Process.Pid
	for delay := time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		var status syscall.WaitStatus
		p.mu.Lock()
		reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if reaped == 0 && (err == nil || errors.Is(err, syscall.EINTR)) {
			p.mu.Unlock()
			time.Sleep(delay)
			continue
		}
		// An error here says the leader is no child to wait for any
		// more, so no kill may reach its id either.
		p.reaped = true
		p.mu.Unlock()
		_ = p.cmd.Process.Release()
		close(p.exited)
		if err != nil {
			return err
		}
		if status.Exited() && status.ExitStatus() == 0 {
			return nil
		}
		return exitStatus(status)
	}
}

// exitStatus is how an agent reaped by reapRunning ended, which it tells as
// exec.ExitError does.
type exitStatus syscall.WaitStatus

func (s exitStatus) Error() string {
	status := syscall.WaitStatus(s)
	if status.Signaled() {
		return "signal: " + status.Signal().String()
	}
	return "exit status " + strconv.Itoa(status.ExitStatus())
}

// ExitCode is the agent's exit code, or -1 if a signal ended it.
func (s exitStatus) ExitCode() int {
	status := syscall.WaitStatus(s)
	if !status.Exited() {
		return -1
	}
	return status.ExitStatus()
}

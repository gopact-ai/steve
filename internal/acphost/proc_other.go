//go:build !unix

package acphost

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}

// reapRunning reaps an agent whose exit cannot be watched without reaping
// it. No kill here signals a group's id, so none can reach one let go.
func (p *localProcess) reapRunning() error {
	err := exitError(p.cmd.Process.Wait())
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	close(p.exited)
	return err
}

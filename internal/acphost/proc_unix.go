//go:build unix

package acphost

import (
	"os/exec"
	"syscall"
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

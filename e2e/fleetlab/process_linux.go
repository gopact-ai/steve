//go:build linux

package fleetlab

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type ownedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startOwned(cmd *exec.Cmd, logPath string) (*ownedProcess, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	p := &ownedProcess{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); log.Close(); close(p.done) }()
	return p, nil
}

func (p *ownedProcess) stop() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		err = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		<-p.done
		return fmt.Errorf("hub required SIGKILL after 10 seconds of shutdown")
	}
}

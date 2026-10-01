//go:build linux

package peerstop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func stopInstallation(parent context.Context, sidecar, cluster, node string) (bool, error) {
	return stopInstallationWithin(parent, sidecar, cluster, node, 30*time.Second)
}

func stopInstallationWithin(parent context.Context, sidecar, cluster, node string, grace time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 35*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	instance, err := loadInstallation(sidecar, cluster, node)
	if err != nil {
		return false, err
	}
	file, err := privateFile(filepath.Join(instance.root, "cluster", "peer-process", "gateway.lock"))
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return false, instance.confirmEmpty(file)
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		return false, fmt.Errorf("%w: installation lock unavailable", ErrUnproven)
	}
	pid, err := lockOwner(file)
	if err != nil {
		return false, err
	}
	before, found, err := instance.process(pid)
	if err != nil || !found {
		return false, fmt.Errorf("%w: lock owner is not the installed peer", ErrUnproven)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return false, ErrUnsupported
	}
	if err != nil {
		return false, fmt.Errorf("%w: stable process handle unavailable: %v", ErrUnproven, err)
	}
	defer unix.Close(fd)
	after, found, err := instance.process(pid)
	if err != nil || !found || after != before {
		return false, fmt.Errorf("%w: original peer identity changed", ErrUnproven)
	}
	held, err := lockOwner(file)
	if err != nil || held != pid {
		return false, fmt.Errorf("%w: original peer no longer owns its installation", ErrUnproven)
	}
	if err := instance.verify(); err != nil {
		return false, err
	}
	if err := signalHandle(fd, unix.SIGTERM); err != nil {
		return false, err
	}
	graceful, stopGrace := context.WithTimeout(ctx, grace)
	err = waitHandle(graceful, fd)
	stopGrace()
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("%w: %v", ErrRunning, ctx.Err())
		}
		if err := signalHandle(fd, unix.SIGKILL); err != nil {
			return false, err
		}
		if err := waitHandle(ctx, fd); err != nil {
			return false, fmt.Errorf("%w: %v", ErrRunning, err)
		}
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return false, fmt.Errorf("%w: installation lock was acquired by another process", ErrUnproven)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return true, instance.confirmEmpty(file)
}

func (i installation) confirmEmpty(file *os.File) error {
	if err := sameLock(file); err != nil {
		return err
	}
	if err := i.verify(); err != nil {
		return err
	}
	return i.noPeer()
}
func signalHandle(fd int, signal unix.Signal) error {
	err := unix.PidfdSendSignal(fd, signal, nil, 0)
	if errors.Is(err, unix.ENOSYS) {
		return ErrUnsupported
	}
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
func waitHandle(ctx context.Context, fd int) error {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Poll(poll, 20)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 && poll[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		if n > 0 {
			return ErrUnproven
		}
	}
}

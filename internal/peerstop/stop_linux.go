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
	if err := checkProcessNamespace(); err != nil {
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
		return false, instance.confirmEmpty(ctx, file)
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		return false, fmt.Errorf("%w: installation lock unavailable", ErrUnproven)
	}
	pid, err := lockOwner(file)
	if err != nil {
		return false, err
	}
	fd, err := pinPeer(file, pid, processOps{identity: instance.process, owner: lockOwner, open: func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }, close: unix.Close})
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	if err := instance.verify(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
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
	return true, instance.confirmEmpty(ctx, file)
}

func (i installation) confirmEmpty(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sameLock(file); err != nil {
		return err
	}
	if err := i.verify(); err != nil {
		return err
	}
	return i.noPeer(ctx)
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

type processOps struct {
	identity func(int) (processIdentity, bool, error)
	owner    func(*os.File) (int, error)
	open     func(int) (int, error)
	close    func(int) error
}

func pinPeer(file *os.File, pid int, k processOps) (int, error) {
	before, found, err := k.identity(pid)
	if err != nil || !found {
		return -1, fmt.Errorf("%w: lock owner is not the installed peer", ErrUnproven)
	}
	fd, err := k.open(pid)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return -1, ErrUnsupported
	}
	if err != nil {
		return -1, fmt.Errorf("%w: stable process handle unavailable: %v", ErrUnproven, err)
	}
	after, found, err := k.identity(pid)
	if err != nil || !found || after != before {
		k.close(fd)
		return -1, fmt.Errorf("%w: original peer identity changed", ErrUnproven)
	}
	owner, err := k.owner(file)
	if err != nil || owner != pid {
		k.close(fd)
		return -1, fmt.Errorf("%w: original peer no longer owns its installation", ErrUnproven)
	}
	return fd, nil
}

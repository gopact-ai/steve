package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
)

// Here reports where this process runs.
func Here() (Place, error) {
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return Place{}, fmt.Errorf("read boot session: %w", err)
	}
	if boot == "" {
		return Place{}, errors.New("boot session is empty")
	}
	place := Place{Boot: boot}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if data, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		if match := platformUUID.FindSubmatch(data); len(match) == 2 {
			place.Machine = machineDigest(string(match[1]))
		}
	}
	return place, nil
}

var platformUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([[:xdigit:]-]{36})"`)

// Capture reads the identity of the group pid leads, pid being a child of
// this process that has not been waited for.
func Capture(pid int, mark string) (Identity, error) {
	p, found, err := status(pid)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		return Identity{}, fmt.Errorf("process %d is gone", pid)
	}
	if p.group != pid {
		return Identity{}, fmt.Errorf("process %d does not lead its process group", pid)
	}
	return Identity{Group: pid, Leader: pid, Start: p.start, Mark: mark}, nil
}

// WaitExit returns once the child pid has exited, leaving it unreaped: it
// keeps its pid, and so its group's id, until it is waited for.
func WaitExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var change unix.Kevent_t
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	changes := []unix.Kevent_t{change}
	events := make([]unix.Kevent_t, 1)
	for {
		timeout := unix.NsecToTimespec(int64(time.Second))
		n, err := unix.Kevent(kq, changes, events, &timeout)
		changes = nil
		switch {
		case errors.Is(err, unix.ESRCH):
			// The child already exited.
			return nil
		case err != nil && !errors.Is(err, unix.EINTR):
			return err
		case n > 0 && events[0].Flags&unix.EV_ERROR != 0:
			if errno := unix.Errno(events[0].Data); errno != unix.ESRCH {
				return errno
			}
			return nil
		case n > 0:
			return nil
		}
		// A wait that saw nothing looks at the child itself, so an exit
		// the queue missed is not waited for forever.
		p, found, err := status(pid)
		if err != nil {
			return err
		}
		if !found || !p.live {
			return nil
		}
	}
}

// Live reports whether any process in the group that this process can see
// still runs. A zombie runs nothing and does not count. A member hidden from
// this process is not listed, so false does not show the group is empty;
// Gone does.
func Live(group int) (bool, error) {
	members, err := members(group)
	return len(members) > 0, err
}

func status(pid int) (process, bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return process{}, false, err
	}
	for _, proc := range procs {
		if int(proc.Proc.P_pid) == pid {
			return describe(proc), true, nil
		}
	}
	return process{}, false, nil
}

// zombie is the state of a process that exited and was not yet waited for.
const zombie = 5

func describe(proc unix.KinfoProc) process {
	start := proc.Proc.P_starttime
	return process{
		pid:   int(proc.Proc.P_pid),
		start: uint64(start.Sec)*1_000_000 + uint64(start.Usec),
		group: int(proc.Eproc.Pgid),
		live:  proc.Proc.P_stat != zombie,
	}
}

// members lists the running processes of a group.
func members(group int) ([]process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if err != nil {
		return nil, err
	}
	var found []process
	for _, proc := range procs {
		if p := describe(proc); p.live && p.group == group {
			found = append(found, p)
		}
	}
	return found, nil
}

func carriesMark(pid int, mark string) bool {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return false
	}
	environ, ok := procargsEnvironment(raw)
	return ok && hasMark(environ, mark)
}

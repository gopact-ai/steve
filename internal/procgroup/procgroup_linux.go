package procgroup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Here reports where this process runs.
func Here() (Place, error) {
	// Everything below reads /proc as this process's own pid namespace
	// sees it; a /proc of another namespace would name other processes.
	self, err := os.Readlink("/proc/self")
	if err != nil {
		return Place{}, fmt.Errorf("read /proc/self: %w", err)
	}
	if self != strconv.Itoa(os.Getpid()) {
		return Place{}, errors.New("/proc belongs to another pid namespace")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Place{}, fmt.Errorf("read boot id: %w", err)
	}
	namespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return Place{}, fmt.Errorf("read pid namespace: %w", err)
	}
	place := Place{Boot: strings.TrimSpace(string(boot)), Namespace: namespace}
	if place.Boot == "" {
		return Place{}, errors.New("boot id is empty")
	}
	if raw, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(raw)); machineID.MatchString(id) {
			place.Machine = machineDigest(id)
		}
	}
	return place, nil
}

var machineID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

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
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
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
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return process{}, false, nil
	}
	if err != nil {
		return process{}, false, err
	}
	p, err := parseStat(pid, raw)
	return p, err == nil, err
}

// parseStat reads the fields of /proc/<pid>/stat after the command name,
// which is in parentheses and may itself hold any byte.
func parseStat(pid int, raw []byte) (process, error) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return process{}, fmt.Errorf("process %d: malformed stat", pid)
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return process{}, fmt.Errorf("process %d: short stat", pid)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return process{}, fmt.Errorf("process %d: group: %w", pid, err)
	}
	threads, err := strconv.Atoi(fields[17])
	if err != nil {
		return process{}, fmt.Errorf("process %d: threads: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return process{}, fmt.Errorf("process %d: start time: %w", pid, err)
	}
	// The state is its first thread's: a process whose first thread has
	// ended shows as a zombie while its other threads still run.
	exited := (fields[0] == "Z" || fields[0] == "X") && threads <= 1
	return process{pid: pid, start: start, group: group, live: !exited}, nil
}

// members lists the running processes of a group.
func members(group int) ([]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var found []process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, ok, err := status(pid)
		if errors.Is(err, os.ErrPermission) {
			// /proc mounted with hidepid shows nothing of another user's
			// processes. Such a member is not listed; whether a group is
			// empty is asked of the kernel instead.
			continue
		}
		if err != nil {
			return nil, err
		}
		if ok && p.live && p.group == group {
			found = append(found, p)
		}
	}
	return found, nil
}

func carriesMark(pid int, mark string) bool {
	environ, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	return err == nil && hasMark(environ, mark)
}

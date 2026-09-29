package procgroup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
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

// members lists the processes of a group, zombies included.
func members(group int) (listing, error) { return procPasses.list(group) }

// procPasses reads the processes /proc shows.
var procPasses = &passes{read: readProc}

// passes lists groups by passes over every process, which read calls for.
type passes struct {
	read func() (pass, error)
}

// pass is what one read of every process shows: the processes of each
// group, and whether the read misses no process.
type pass struct {
	groups   map[int][]process
	complete bool
}

// list lists the processes of a group by a pass that begins after it is
// called.
func (s *passes) list(group int) (listing, error) {
	read, err := s.read()
	if err != nil {
		return listing{}, err
	}
	found := listing{members: slices.Clone(read.groups[group]), complete: read.complete}
	slices.SortFunc(found.members, byPid)
	return found, nil
}

// readProc reads every process /proc shows.
func readProc() (pass, error) {
	read := pass{groups: map[int][]process{}, complete: procShowsAll()}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return pass{}, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, ok, err := status(pid)
		if errors.Is(err, os.ErrPermission) {
			// /proc mounted with hidepid shows nothing of another user's
			// processes: such a member is not listed, and the listing can
			// miss one.
			read.complete = false
			continue
		}
		if err != nil {
			return pass{}, err
		}
		if ok {
			read.groups[p.group] = append(read.groups[p.group], p)
		}
	}
	return read, nil
}

// procShowsAll reports whether /proc shows every process of this process's
// pid namespace: it is that namespace's, and it is not mounted with
// hidepid, which hides other users' processes, or has it off.
func procShowsAll() bool {
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return false
	}
	mount, err := procMount()
	if err != nil {
		return false
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	return err == nil && mountShowsAll(mountinfo, mount)
}

// procMount returns the id of the mount /proc resolves to, the one a
// listing of /proc reads, as /proc/self/mountinfo numbers mounts.
func procMount() (string, error) {
	dir, err := os.Open("/proc")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	info, err := os.ReadFile("/proc/self/fdinfo/" + strconv.FormatUint(uint64(dir.Fd()), 10))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(info), "\n") {
		if id, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strings.TrimSpace(id), nil
		}
	}
	return "", errors.New("the kernel names no mount for /proc")
}

// mountShowsAll reports whether, in the mount table in the format of
// /proc/self/mountinfo, the mount whose id is mount is a proc filesystem on
// /proc without hidepid on. Other mounts on /proc, which /proc does not
// resolve to, do not count.
func mountShowsAll(mountinfo []byte, mount string) bool {
	for line := range strings.SplitSeq(string(mountinfo), "\n") {
		// Fields are: id, parent, device, root, mount point, mount
		// options, optional fields, "-", filesystem, source and the
		// filesystem's options.
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != mount {
			continue
		}
		dash := slices.Index(fields, "-")
		return fields[4] == "/proc" && dash >= 6 && len(fields) >= dash+4 && fields[dash+1] == "proc" &&
			!hidesPids(fields[5]) && !hidesPids(fields[dash+3])
	}
	return false
}

// hidesPids reports whether mount options turn hidepid on.
func hidesPids(options string) bool {
	for option := range strings.SplitSeq(options, ",") {
		if value, ok := strings.CutPrefix(option, "hidepid="); ok && value != "0" && value != "off" {
			return true
		}
	}
	return false
}

func carriesMark(pid int, mark string) bool {
	environ, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	return err == nil && hasMark(environ, mark)
}

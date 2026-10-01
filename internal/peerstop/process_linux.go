//go:build linux

package peerstop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	pid   int
	start uint64
}

func lockOwner(file *os.File) (int, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	locks, err := os.Open("/proc/locks")
	if err != nil {
		return 0, fmt.Errorf("%w: kernel lock owners unavailable", ErrUnproven)
	}
	defer locks.Close()
	owner := 0
	scan := bufio.NewScanner(locks)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) > 1 && fields[1] == "->" {
			continue
		}
		if len(fields) < 8 {
			return 0, fmt.Errorf("%w: incomplete kernel lock record", ErrUnproven)
		}
		key := strings.Split(fields[5], ":")
		if len(key) != 3 {
			return 0, ErrUnproven
		}
		major, e1 := strconv.ParseUint(key[0], 16, 32)
		minor, e2 := strconv.ParseUint(key[1], 16, 32)
		inode, e3 := strconv.ParseUint(key[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			return 0, ErrUnproven
		}
		if major != uint64(unix.Major(uint64(stat.Dev))) || minor != uint64(unix.Minor(uint64(stat.Dev))) || inode != stat.Ino {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || fields[1] != "FLOCK" || fields[3] != "WRITE" || pid <= 1 || pid == os.Getpid() || owner != 0 {
			return 0, fmt.Errorf("%w: ambiguous installation lock ownership", ErrUnproven)
		}
		owner = pid
	}
	if err := scan.Err(); err != nil {
		return 0, err
	}
	if owner == 0 {
		return 0, fmt.Errorf("%w: held installation lock has no visible owner", ErrUnproven)
	}
	if err := sameLock(file); err != nil {
		return 0, err
	}
	if err := ownsKernelLock(file, owner); err != nil {
		return 0, err
	}
	return owner, nil
}

func (i installation) process(pid int) (processIdentity, bool, error) {
	zero := processIdentity{}
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return zero, false, ErrUnproven
	}
	if stat.Uid != uint32(os.Getuid()) {
		return zero, false, nil
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return zero, false, err
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) < 2 || args[1] != "peer" {
		return zero, false, nil
	}
	exe, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("%w: peer executable cannot be checked", ErrUnproven)
	}
	if !os.SameFile(i.binary, exe) {
		return zero, false, nil
	}
	if err := checkPeerEnvironment(pid); err != nil {
		return zero, false, err
	}
	config, sidecar, err := peerConfiguration(args[2:])
	if err != nil {
		return zero, false, err
	}
	cwd := ""
	if !filepath.IsAbs(config) || !filepath.IsAbs(sidecar) {
		cwd, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			return zero, false, fmt.Errorf("%w: peer working directory cannot be checked", ErrUnproven)
		}
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		canonical, e := filepath.EvalSymlinks(path)
		if e != nil {
			return ""
		}
		return canonical
	}
	if resolve(config) != filepath.Join(i.root, "config.json") || resolve(sidecar) != i.sidecar {
		return zero, false, nil
	}
	raw, err = os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return zero, false, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return zero, false, ErrUnproven
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return zero, false, ErrUnproven
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return zero, false, ErrUnproven
	}
	return processIdentity{pid, start}, true, nil
}

func (i installation) noPeer(ctx context.Context) error {
	if err := checkProcessNamespace(); err != nil {
		return err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		_, matches, err := i.process(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		if matches {
			return fmt.Errorf("%w: installation peer exists without its lock", ErrUnproven)
		}
	}
	return nil
}

func checkProcessNamespace() error {
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return fmt.Errorf("%w: process namespace differs", ErrUnproven)
	}
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("%w: process namespace cannot be checked", ErrUnproven)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, "NSpid:"); found {
			fields := strings.Fields(value)
			if len(fields) == 1 && fields[0] == self {
				return nil
			}
			return fmt.Errorf("%w: process namespace differs", ErrUnproven)
		}
	}
	return fmt.Errorf("%w: process namespace is unknown", ErrUnproven)
}

// The kernel's reported PID alone is not enough for an inherited lock after
// its original holder exits. The identified process must still own a file
// description carrying that exact flock, not merely have the inode open.
func ownsKernelLock(file *os.File, pid int) error {
	expected, err := file.Stat()
	if err != nil {
		return err
	}
	root := fmt.Sprintf("/proc/%d", pid)
	entries, err := os.ReadDir(filepath.Join(root, "fd"))
	if err != nil {
		return fmt.Errorf("%w: lock owner's descriptors unavailable", ErrUnproven)
	}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(root, "fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: lock owner's descriptor cannot be checked", ErrUnproven)
		}
		if !os.SameFile(expected, info) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, "fdinfo", entry.Name()))
		if err != nil {
			return fmt.Errorf("%w: lock description unavailable", ErrUnproven)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			value, found := strings.CutPrefix(line, "lock:")
			if !found {
				continue
			}
			fields := strings.Fields(value)
			if len(fields) >= 8 && fields[1] == "FLOCK" && fields[3] == "WRITE" && fields[4] == strconv.Itoa(pid) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: kernel PID does not own the installation flock", ErrUnproven)
}

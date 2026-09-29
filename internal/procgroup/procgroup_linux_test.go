package procgroup

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// adoptOrphans makes this process, for as long as the test runs, the
// reaper of the orphans its children leave, as a PID 1 is. It reaps none of
// them unless asked, as a PID 1 that reaps no orphans does not.
func adoptOrphans(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })
}

// reapOrphan kills, when the test ends, the orphan pid if it still runs
// with the test's own argument, and reaps it, as it was left to this
// process.
func reapOrphan(t *testing.T, pid int, arg string) {
	t.Cleanup(func() {
		if cmdlineIs(pid, "sleep", arg) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if reaped, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); reaped == pid || err != nil {
				return
			}
		}
	})
}

// A PID 1 that reaps no orphans, as a container's often does not, leaves the
// members of a group whose node was killed zombies once they end. Nothing
// of the group runs, so its stop is confirmed, though nothing shows the
// group is the recorded one: a zombie carries no mark to read.
func TestSettleConfirmsAGroupOnlyZombieMembersAreLeftIn(t *testing.T) {
	adoptOrphans(t)
	member, id, arg := orphan(t, "other", "m")
	reapOrphan(t, member, arg)
	if err := syscall.Kill(member, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitZombie(t, member)
	signals := 0
	if err := settle(id, here(t), here(t), time.Second, counting(&signals)); err != nil || signals > 0 {
		t.Fatalf("settle = %v after %d signals, want nil after none", err, signals)
	}
}

// Where the listing of processes can miss one, as under /proc mounted with
// hidepid, zombies do not show that nothing else is left in a group: its
// stop is confirmed only once the kernel finds no process in it.
func TestSettleDoesNotConfirmZombiesWhereTheListingCanMissProcesses(t *testing.T) {
	cmd, id := startGroup(t, "m", "m", `exec sleep "$1"`, pause())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	awaitZombie(t, cmd.Process.Pid)
	hiding := kernel{gone: gone, list: func(group int) (listing, error) {
		found, err := members(group)
		found.complete = false
		return found, err
	}, kill: Kill}
	if err := settle(id, here(t), here(t), 200*time.Millisecond, hiding); !errors.Is(err, ErrRunning) {
		t.Fatalf("settle = %v, want %v", err, ErrRunning)
	}
	_ = cmd.Wait()
	if err := settle(id, here(t), here(t), 5*time.Second, hiding); err != nil {
		t.Fatal(err)
	}
}

// /proc shows every process only as the proc filesystem last mounted on it,
// without hidepid or with it off.
func TestMountShowsAllProcessesOnlyWithoutHidepid(t *testing.T) {
	mount := func(point, options, filesystem, superOptions string) string {
		return fmt.Sprintf("21 25 0:20 / %s %s shared:13 - %s %s %s\n", point, options, filesystem, filesystem, superOptions)
	}
	for _, tc := range []struct {
		name      string
		mountinfo string
		shows     bool
	}{
		{"without hidepid", mount("/proc", "rw,nosuid", "proc", "rw"), true},
		{"with hidepid 0", mount("/proc", "rw", "proc", "rw,hidepid=0"), true},
		{"with hidepid off", mount("/proc", "rw", "proc", "rw,hidepid=off"), true},
		{"with hidepid 2", mount("/proc", "rw", "proc", "rw,hidepid=2,gid=10"), false},
		{"with hidepid invisible", mount("/proc", "rw", "proc", "rw,hidepid=invisible"), false},
		{"with hidepid among the mount options", mount("/proc", "rw,hidepid=1", "proc", "rw"), false},
		{"without optional fields", "21 25 0:20 / /proc rw - proc proc rw,hidepid=2\n", false},
		{"hidden by a later mount", mount("/proc", "rw", "proc", "rw") + mount("/proc", "rw", "proc", "rw,hidepid=2"), false},
		{"shown by a later mount", mount("/proc", "rw", "proc", "rw,hidepid=2") + mount("/proc", "rw", "proc", "rw"), true},
		{"with hidepid on another mount", mount("/proc", "rw", "proc", "rw") + mount("/srv/proc", "rw", "proc", "rw,hidepid=2"), true},
		{"with another filesystem on /proc", mount("/proc", "rw", "tmpfs", "rw"), false},
		{"without /proc", mount("/sys", "rw", "sysfs", "rw"), false},
	} {
		if shows := mountShowsAll([]byte(tc.mountinfo), "21"); shows != tc.shows {
			t.Errorf("%s: shows all %v, want %v", tc.name, shows, tc.shows)
		}
	}
}

// A process whose first thread has ended shows as a zombie while its other
// threads still run. Only a zombie with no thread left runs nothing.
func TestParseStatTakesAZombieWithThreadsLeftForRunning(t *testing.T) {
	for _, tc := range []struct {
		state   string
		threads int
		live    bool
	}{
		{"S", 1, true},
		{"Z", 1, false},
		{"X", 1, false},
		{"Z", 3, true},
	} {
		raw := fmt.Appendf(nil, "42 (a) b) %s 1 42 42 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 %d 0 777", tc.state, tc.threads)
		p, err := parseStat(42, raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.live != tc.live || p.group != 42 || p.start != 777 {
			t.Errorf("state %s with %d threads: %+v, want live %v", tc.state, tc.threads, p, tc.live)
		}
	}
}

package procgroup

import (
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
	if err := Settle(id, here(t), here(t), time.Second); err != nil {
		t.Fatal(err)
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

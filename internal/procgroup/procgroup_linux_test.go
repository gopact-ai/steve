package procgroup

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// /proc shows every process only as the proc filesystem it resolves to is
// mounted: without hidepid or with it off. Other mounts on /proc, whether
// mounted before or after it, do not count.
func TestMountShowsAllProcessesOnlyWithoutHidepid(t *testing.T) {
	mount := func(id, point, options, filesystem, superOptions string) string {
		return fmt.Sprintf("%s 25 0:20 / %s %s shared:13 - %s %s %s\n", id, point, options, filesystem, filesystem, superOptions)
	}
	for _, tc := range []struct {
		name      string
		mountinfo string
		shows     bool
	}{
		{"without hidepid", mount("21", "/proc", "rw,nosuid", "proc", "rw"), true},
		{"with hidepid 0", mount("21", "/proc", "rw", "proc", "rw,hidepid=0"), true},
		{"with hidepid off", mount("21", "/proc", "rw", "proc", "rw,hidepid=off"), true},
		{"with hidepid 2", mount("21", "/proc", "rw", "proc", "rw,hidepid=2,gid=10"), false},
		{"with hidepid invisible", mount("21", "/proc", "rw", "proc", "rw,hidepid=invisible"), false},
		{"with hidepid among the mount options", mount("21", "/proc", "rw,hidepid=1", "proc", "rw"), false},
		{"without optional fields", "21 25 0:20 / /proc rw - proc proc rw,hidepid=2\n", false},
		{"with hidepid, another mounted on /proc later", mount("21", "/proc", "rw", "proc", "rw,hidepid=2") + mount("30", "/proc", "rw", "proc", "rw"), false},
		{"without hidepid, another mounted on /proc later", mount("21", "/proc", "rw", "proc", "rw") + mount("30", "/proc", "rw", "proc", "rw,hidepid=2"), true},
		{"with hidepid, another mounted on /proc earlier", mount("30", "/proc", "rw", "proc", "rw") + mount("21", "/proc", "rw", "proc", "rw,hidepid=2"), false},
		{"with hidepid on another mount", mount("21", "/proc", "rw", "proc", "rw") + mount("30", "/srv/proc", "rw", "proc", "rw,hidepid=2"), true},
		{"with another filesystem on /proc", mount("21", "/proc", "rw", "tmpfs", "rw"), false},
		{"without the mount /proc resolves to", mount("30", "/proc", "rw", "proc", "rw"), false},
		{"without /proc", mount("21", "/sys", "rw", "sysfs", "rw"), false},
	} {
		if shows := mountShowsAll([]byte(tc.mountinfo), "21"); shows != tc.shows {
			t.Errorf("%s: shows all %v, want %v", tc.name, shows, tc.shows)
		}
	}
}

// The mount /proc resolves to is found among the mounts, as the proc
// filesystem on /proc.
func TestProcMountIsTheProcFilesystemOnProc(t *testing.T) {
	mount, err := procMount()
	if err != nil {
		t.Fatal(err)
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != mount {
			continue
		}
		dash := slices.Index(fields, "-")
		if fields[4] != "/proc" || dash < 0 || len(fields) <= dash+1 || fields[dash+1] != "proc" {
			t.Fatalf("/proc resolves to mount %s, which is not the proc filesystem on /proc: %s", mount, line)
		}
		return
	}
	t.Fatalf("/proc resolves to mount %s, which the mount table does not list", mount)
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

// Listings asked for while a read of every process is under way share the
// next read, so settling many groups at once does not read every process
// once for each of them. Each is of a read that began after it was asked
// for, as the one under way may have missed what changed since.
func TestListingsAskedForTogetherShareOneRead(t *testing.T) {
	var reads atomic.Int32
	began, finish := make(chan struct{}), make(chan struct{})
	s := &passes{read: func() (pass, error) {
		if reads.Add(1) == 1 {
			close(began)
			<-finish
			return pass{groups: map[int][]process{7: {{pid: 7, group: 7, live: true}}}, complete: true}, nil
		}
		return pass{groups: map[int][]process{7: {{pid: 8, group: 7, live: true}}}, complete: true}, nil
	}}
	first := make(chan listing, 1)
	go func() {
		found, _ := s.list(7)
		first <- found
	}()
	<-began
	const later = 50
	found := make([]listing, later)
	var asked, done sync.WaitGroup
	for i := range later {
		asked.Add(1)
		done.Go(func() {
			asked.Done()
			found[i], _ = s.list(7)
		})
	}
	asked.Wait()
	// As long as a listing takes to be asked for once it is called.
	time.Sleep(100 * time.Millisecond)
	close(finish)
	done.Wait()
	if got := <-first; len(got.members) != 1 || got.members[0].pid != 7 {
		t.Fatalf("the first listing is %+v, want the read it began", got.members)
	}
	for i, got := range found {
		if len(got.members) != 1 || got.members[0].pid != 8 {
			t.Fatalf("listing %d is %+v, want a read that began after it was asked for", i, got.members)
		}
	}
	if n := reads.Load(); n > 2 {
		t.Fatalf("%d listings asked for together read every process %d times, want at most 2", later+1, n)
	}
}

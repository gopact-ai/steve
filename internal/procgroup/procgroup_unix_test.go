//go:build linux || darwin

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pause is an argument unique to one test, so cleanup never signals a process
// the test did not start.
func pause() string {
	return fmt.Sprintf("%d.%06d", 5000+os.Getpid()%997, time.Now().Nanosecond()/1000)
}

// startGroup starts script under /bin/sh leading a process group of its own,
// its environment carrying carried as the mark, and reads its identity as
// recorded with mark.
func startGroup(t *testing.T, carried, mark, script string, args ...string) (*exec.Cmd, Identity) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()
	if carried != "" {
		cmd.Env = append(cmd.Env, MarkVariable+"="+carried)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := Capture(cmd.Process.Pid, mark)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	return cmd, id
}

// orphan starts a group whose leader leaves a member behind and is reaped, as
// an agent is once the node that started it is gone. It returns the member.
func orphan(t *testing.T, carried, mark string) (int, Identity, string) {
	t.Helper()
	arg, file := pause(), filepath.Join(t.TempDir(), "member")
	cmd, id := startGroup(t, carried, mark, `sleep "$1" </dev/null >/dev/null 2>&1 & echo $! > "$2"`, arg, file)
	endMember(t, file, arg)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !running(member) {
		t.Fatal("the group left no member running")
	}
	return member, id, arg
}

// endMember kills, when the test ends, the member whose pid the group wrote
// to file, if it still runs with the test's own argument. It is registered
// before anything can fail, so a failing test leaves no member behind.
func endMember(t *testing.T, file, arg string) {
	t.Cleanup(func() {
		raw, _ := os.ReadFile(file)
		if member, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && member > 0 && cmdlineIs(member, "sleep", arg) {
			_ = syscall.Kill(member, syscall.SIGKILL)
		}
	})
}

func here(t *testing.T) Place {
	t.Helper()
	place, err := Here()
	if err != nil {
		t.Fatal(err)
	}
	return place
}

// While the recorded leader runs, the group is the recorded one: all of it
// is ended.
func TestSettleEndsTheGroupOfARunningLeader(t *testing.T) {
	arg, file := pause(), filepath.Join(t.TempDir(), "member")
	cmd, id := startGroup(t, "m", "m", `sleep "$1" </dev/null >/dev/null 2>&1 & echo $! > "$2"; exec sleep "$1"`, arg, file)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	endMember(t, file, arg)
	var member int
	for deadline := time.Now().Add(10 * time.Second); member == 0 || !cmdlineIs(cmd.Process.Pid, "sleep", arg); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the group did not start its member")
		}
		raw, _ := os.ReadFile(file)
		member, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if err := Settle(id, here(t), here(t), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if running(member) || running(cmd.Process.Pid) {
		t.Fatal("the stop was confirmed while the group still ran")
	}
}

// A process holding the recorded leader's pid with another start time is not
// the leader, and the recorded group ended before the pid was given to it.
func TestSettleLeavesAProcessThatReusedTheLeadersPidAlone(t *testing.T) {
	arg := pause()
	cmd, id := startGroup(t, "m", "m", `exec sleep "$1"`, arg)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id.Start--
	if err := Settle(id, here(t), here(t), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !running(cmd.Process.Pid) {
		t.Fatal("a process that only reused the recorded pid was signalled")
	}
}

// Once the leader is gone, a group of the recorded id is shown to be the
// recorded one by the mark its members inherited.
func TestSettleEndsAMarkedGroupItsLeaderLeft(t *testing.T) {
	member, id, _ := orphan(t, "m", "m")
	if err := Settle(id, here(t), here(t), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if running(member) {
		t.Fatal("the stop was confirmed while a member still ran")
	}
}

// Without the leader and without the mark, nothing shows the group is the
// recorded one, so nothing is signalled and the stop stays unconfirmed.
func TestSettleLeavesAnUnmarkedGroupAlone(t *testing.T) {
	member, id, _ := orphan(t, "other", "m")
	if err := Settle(id, here(t), here(t), 5*time.Second); !errors.Is(err, ErrUnproven) {
		t.Fatalf("Settle = %v, want %v", err, ErrUnproven)
	}
	if !running(member) {
		t.Fatal("a group not shown to be the recorded one was signalled")
	}
}

// A group that is gone needs nothing ended.
func TestSettleConfirmsAGroupThatIsGone(t *testing.T) {
	cmd, id := startGroup(t, "m", "m", `exit 0`)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := Settle(id, here(t), here(t), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// cmdlineIs reads the arguments pid runs with as ps shows them, which both
// Linux and macOS do alike.
func cmdlineIs(pid int, argv ...string) bool {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.TrimSpace(string(out)) == strings.Join(argv, " ")
}

// running is false for a pid that is gone or a zombie: neither runs.
func running(pid int) bool {
	p, found, err := status(pid)
	return err == nil && found && p.live
}

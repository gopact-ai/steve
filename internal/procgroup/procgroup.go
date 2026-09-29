// Package procgroup ends the process group an agent leads and confirms that
// nothing in it still runs: while its leader is a child of this process,
// and after this process has been restarted, from what was recorded about
// the group when it started.
//
// A pid, and so a process group's id, is given out again once nothing holds
// it. Nothing is signalled here unless it is shown to be the recorded
// group: while any process still has a pid, as its own pid or as its
// group's id, the kernel gives that pid to no new process, so a recorded
// leader that still holds its pid with its recorded start time proves the
// group is the recorded one, and a pid holder with another start time
// proves the recorded group emptied before that process started.
package procgroup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// MarkVariable names the environment variable that carries a group's mark
// to its leader, and from it to what it spawns.
const MarkVariable = "STEVE_PROCESS_MARK"

var (
	// ErrUnsupported is returned where this platform cannot name a process
	// group durably or wait for its leader without reaping it.
	ErrUnsupported = errors.New("process groups are not tracked on this platform")
	// ErrUnproven is returned when what runs under a recorded group's id
	// cannot be shown to be the recorded group. Nothing is signalled.
	ErrUnproven = errors.New("process group cannot be shown to be the recorded one")
	// ErrRunning is returned when members of a recorded group still run
	// after being killed, as a process in uninterruptible sleep does until
	// it wakes, or its exited leader is still not reaped.
	ErrRunning = errors.New("process group still has members after SIGKILL")
)

// Identity names a process group this program started: its leader, whose
// pid is the group's id, when the leader started, and the mark the
// leader's environment carries.
type Identity struct {
	Group  int    `json:"group"`
	Leader int    `json:"leader"`
	Start  uint64 `json:"start"`
	Mark   string `json:"mark"`
}

// Place is where processes run. Pids and start times compare only within
// one boot of one machine and one pid namespace.
type Place struct {
	Machine   string `json:"machine,omitempty"`
	Boot      string `json:"boot"`
	Namespace string `json:"namespace,omitempty"`
}

// NewMark returns a mark no other group carries.
func NewMark() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Settle ends what remains of the group id names, recorded where it ran,
// and returns nil once the group provably has no running member.
//
// It signals the group only while it is shown to be the recorded one: its
// recorded leader still holds its pid with its recorded start time, or,
// the leader gone, every running member started after the leader and one
// carries the recorded mark. It judges again before every signal. The
// group is taken for empty only when the kernel finds no process under its
// id, so a member this process cannot see keeps it unconfirmed. When it
// cannot show which group runs under the id it returns ErrUnproven, and
// when members, or a leader not yet reaped, outlive within after being
// killed, ErrRunning.
func Settle(id Identity, ran, here Place, within time.Duration) error {
	return settle(id, ran, here, within, members)
}

// settle is Settle listing a group's running members with list.
func settle(id Identity, ran, here Place, within time.Duration, list func(group int) ([]process, error)) error {
	if id.Group <= 0 || id.Leader != id.Group || id.Start == 0 || id.Mark == "" {
		return fmt.Errorf("%w: its identity is incomplete", ErrUnproven)
	}
	if ran.Boot == "" || here.Boot == "" {
		return fmt.Errorf("%w: the boot it ran in is unknown", ErrUnproven)
	}
	if ran.Boot != here.Boot {
		// Nothing a machine ran outlives its reboot. The same records read
		// on another machine prove nothing about the one that wrote them.
		if ran.Machine != "" && ran.Machine == here.Machine {
			return nil
		}
		return fmt.Errorf("%w: it was recorded on another machine", ErrUnproven)
	}
	if ran.Namespace != here.Namespace {
		return fmt.Errorf("%w: it was recorded in another pid namespace", ErrUnproven)
	}
	deadline := time.Now().Add(within)
	for delay := time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		owned, err := recorded(id, list)
		if err != nil || !owned {
			return err
		}
		if time.Now().After(deadline) {
			return ErrRunning
		}
		if err := Kill(id.Group); err != nil {
			return err
		}
		time.Sleep(delay)
	}
}

// recorded reports whether a process in the group id names still runs and
// the group is shown to be the recorded one; false means no process of the
// recorded group runs. list names the group's running members.
func recorded(id Identity, list func(group int) ([]process, error)) (bool, error) {
	leader, found, err := status(id.Leader)
	if err != nil {
		return false, err
	}
	if found && leader.start != id.Start {
		// The pid was given out again, which it is not while any process
		// has it as its group's id: the recorded group had emptied.
		return false, nil
	}
	if found {
		// The leader, running or not yet reaped, keeps its pid, so the
		// group is the one it started.
		return true, nil
	}
	members, err := list(id.Group)
	if err != nil {
		return false, err
	}
	if len(members) == 0 {
		gone, err := Gone(id.Group)
		if err != nil || gone {
			return false, err
		}
		return false, fmt.Errorf("%w: a process this one cannot see is still in the group", ErrUnproven)
	}
	marked := false
	for _, member := range members {
		if member.start < id.Start {
			return false, fmt.Errorf("%w: a member started before the recorded leader", ErrUnproven)
		}
		marked = marked || carriesMark(member.pid, id.Mark)
	}
	if !marked {
		return false, fmt.Errorf("%w: no member carries the recorded mark", ErrUnproven)
	}
	return true, nil
}

// process is what one pid shows of the process holding it.
type process struct {
	pid   int
	start uint64
	group int
	// live is false for a zombie, which runs nothing and only waits for
	// its parent to reap it.
	live bool
}

// hasMark reports whether an environment, as NUL-separated entries, carries
// mark.
func hasMark(environ []byte, mark string) bool {
	want := MarkVariable + "=" + mark
	for entry := range strings.SplitSeq(string(environ), "\x00") {
		if entry == want {
			return true
		}
	}
	return false
}

// machineDigest keeps a raw machine identifier out of what is recorded.
func machineDigest(raw string) string {
	digest := sha256.Sum256([]byte("steve/process-machine/v1/" + runtime.GOOS + "/" + strings.ToLower(raw)))
	return hex.EncodeToString(digest[:])
}

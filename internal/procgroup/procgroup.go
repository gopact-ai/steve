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
//
// A group has stopped once nothing of it runs: the kernel finds no process
// in it, or only zombies are left in it, which a PID 1 that reaps no
// orphans leaves. Zombies are told from running processes only where every
// process can be listed, so a member hidden from this process is never
// taken for an ended one.
package procgroup

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
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
	// it wakes, or when zombies are left in it where not every process can
	// be listed.
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
// group has stopped when the kernel finds no process under its id, or when
// only zombies are left under it as Inspect judges, so a member this
// process cannot see keeps it unconfirmed. When it cannot show which group
// runs under the id, or this process runs in the group it would signal, it
// returns ErrUnproven, and when members outlive within after being killed,
// ErrRunning.
func Settle(id Identity, ran, here Place, within time.Duration) error {
	return settle(id, ran, here, within, system)
}

// kernel is what settling asks of the kernel, so a test can stand in for
// one that hides processes, or whose processes change between the calls,
// and see what is signalled.
type kernel struct {
	gone func(group int) (bool, error)
	list func(group int) (listing, error)
	kill func(group int) error
}

// system is the kernel this process runs on.
var system = kernel{gone: gone, list: members, kill: Kill}

// settle is Settle asking k.
func settle(id Identity, ran, here Place, within time.Duration, k kernel) error {
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
		owned, err := recorded(id, k)
		if err != nil || !owned {
			return err
		}
		if ownGroup() == id.Group {
			// A kill of the group would end this process with it.
			return fmt.Errorf("%w: this process runs in it", ErrUnproven)
		}
		if time.Now().After(deadline) {
			return ErrRunning
		}
		if err := k.kill(id.Group); err != nil {
			return err
		}
		time.Sleep(delay)
	}
}

// recorded reports whether a process in the group id names still runs and
// the group is shown to be the recorded one; false means no process of the
// recorded group runs. k finds and lists the group's members.
func recorded(id Identity, k kernel) (bool, error) {
	leader, found, err := status(id.Leader)
	if err != nil {
		return false, err
	}
	if found && leader.start != id.Start {
		// The pid was given out again, which it is not while any process
		// has it as its group's id: the recorded group had emptied.
		return false, nil
	}
	if found && leader.live {
		// The running leader keeps its pid, so the group is the one it
		// started.
		return true, nil
	}
	remains, listed, err := inspect(id.Group, k)
	if err != nil {
		return false, err
	}
	if remains.Ended() {
		// Nothing is signalled and no proof is asked for. Whether or not
		// the zombies left are of the recorded group, no member of the
		// recorded group runs: had its id been given to another group, the
		// recorded group had emptied before.
		return false, nil
	}
	if found {
		// The leader, not yet reaped, keeps its pid, so the group is the
		// one it started.
		return true, nil
	}
	running := listed.running()
	if len(running) == 0 {
		return false, fmt.Errorf("%w: no running member is listed, yet the group is not empty: %v", ErrUnproven, remains)
	}
	marked := false
	for _, member := range running {
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

// Remains is what is left of a process group.
type Remains struct {
	// Gone is true when the kernel finds no process in the group, a zombie
	// included.
	Gone bool
	// Running and Zombies count the members listed that run and those
	// that exited and were not reaped.
	Running, Zombies int
	// Complete is true when the listing misses no process: none is hidden
	// from this process.
	Complete bool
	// changing is true when two listings that each found only zombies did
	// not agree, as the group changed while it was listed.
	changing bool
}

// Ended reports whether nothing of the group runs: the kernel finds no
// process in it, or a listing that misses no process, and a second one that
// agrees with it, find only zombies in it. A zombie runs nothing, and while
// it holds the group's id no other group can be given it.
func (r Remains) Ended() bool {
	return r.Gone || r.Complete && !r.changing && r.Running == 0 && r.Zombies > 0
}

// String says what is left, for a report of a group that does not end.
func (r Remains) String() string {
	if r.Gone {
		return "no process is left in the group"
	}
	listed := fmt.Sprintf("%d running and %d zombie members are listed", r.Running, r.Zombies)
	switch {
	case r.changing:
		return listed + ", and the group changed while it was listed"
	case !r.Complete && r.Running == 0 && r.Zombies > 0:
		return listed + "; zombies are left unreaped, as a PID 1 that reaps no orphans leaves them, and the listing can miss processes, as /proc mounted with hidepid hides them, so the zombies do not show that nothing else is left"
	case !r.Complete:
		return listed + ", and the listing can miss processes, as /proc mounted with hidepid hides them"
	case r.Running == 0 && r.Zombies == 0:
		return listed + ", yet the kernel still finds a process in the group"
	}
	return listed
}

// Inspect reports what is left of a process group.
func Inspect(group int) (Remains, error) {
	remains, _, err := inspect(group, system)
	return remains, err
}

// inspect is Inspect asking k. It returns the listing it judged by too.
func inspect(group int, k kernel) (Remains, listing, error) {
	if gone, err := k.gone(group); err != nil || gone {
		return Remains{Gone: gone}, listing{}, err
	}
	first, err := k.list(group)
	if err != nil {
		return Remains{}, listing{}, err
	}
	if !first.remains().Ended() {
		return first.remains(), first, nil
	}
	// A member that forks and exits while the processes are read can be
	// listed as a zombie while its child, started after the read began, is
	// not listed. A second listing shows that child, or, had it done the
	// same, a zombie the first did not: only a group that no longer
	// changes is listed alike twice.
	second, err := k.list(group)
	if err != nil {
		return Remains{}, listing{}, err
	}
	remains := second.remains()
	remains.changing = !slices.Equal(first.members, second.members)
	return remains, second, nil
}

// listing is what one pass over the processes shows of a group: its
// members, zombies included, in pid order, and whether the pass misses no
// process.
type listing struct {
	members  []process
	complete bool
}

func (l listing) remains() Remains {
	r := Remains{Complete: l.complete}
	for _, member := range l.members {
		if member.live {
			r.Running++
		} else {
			r.Zombies++
		}
	}
	return r
}

func (l listing) running() []process {
	var running []process
	for _, member := range l.members {
		if member.live {
			running = append(running, member)
		}
	}
	return running
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

func byPid(a, b process) int { return cmp.Compare(a.pid, b.pid) }

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

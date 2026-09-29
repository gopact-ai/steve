package procgroup

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// Where a group ran decides what its recorded pids can mean here, before any
// process is looked at.
func TestSettleComparesWhereTheGroupRan(t *testing.T) {
	id := Identity{Group: 4242, Leader: 4242, Start: 7, Mark: "m"}
	here := Place{Machine: "machine", Boot: "boot-2", Namespace: "pid:[1]"}
	for _, tc := range []struct {
		name string
		id   Identity
		ran  Place
		want error
	}{
		{"a reboot of this machine ended everything", id, Place{Machine: "machine", Boot: "boot-1", Namespace: "pid:[1]"}, nil},
		{"another machine", id, Place{Machine: "other", Boot: "boot-1", Namespace: "pid:[1]"}, ErrUnproven},
		{"an unknown machine", id, Place{Boot: "boot-1", Namespace: "pid:[1]"}, ErrUnproven},
		{"an unknown boot", id, Place{Machine: "machine", Namespace: "pid:[1]"}, ErrUnproven},
		{"another pid namespace", id, Place{Machine: "machine", Boot: "boot-2", Namespace: "pid:[2]"}, ErrUnproven},
		{"no recorded leader", Identity{Group: 4242, Start: 7, Mark: "m"}, here, ErrUnproven},
		{"a leader of another group", Identity{Group: 4242, Leader: 4243, Start: 7, Mark: "m"}, here, ErrUnproven},
		{"no start time", Identity{Group: 4242, Leader: 4242, Mark: "m"}, here, ErrUnproven},
		{"no mark", Identity{Group: 4242, Leader: 4242, Start: 7}, here, ErrUnproven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Settle(tc.id, tc.ran, here, time.Second)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Settle = %v, want %v", err, tc.want)
			}
		})
	}
}

// A mark counts only as a whole environment entry, never inside an argument.
func TestProcargsEnvironmentSkipsTheArguments(t *testing.T) {
	procargs := func(argv []string, env string) []byte {
		raw := binary.LittleEndian.AppendUint32(nil, uint32(len(argv)))
		raw = append(raw, "/bin/tool\x00\x00\x00\x00"...)
		for _, arg := range argv {
			raw = append(raw, arg+"\x00"...)
		}
		return append(raw, env...)
	}
	marked := procargs([]string{"tool", "x"}, "HOME=/h\x00"+MarkVariable+"=abc\x00\x00")
	if environ, ok := procargsEnvironment(marked); !ok || !hasMark(environ, "abc") || hasMark(environ, "ab") {
		t.Fatalf("environment %q: ok=%v", environ, ok)
	}
	inArgument := procargs([]string{"tool", MarkVariable + "=abc"}, "HOME=/h\x00\x00")
	if environ, ok := procargsEnvironment(inArgument); !ok || hasMark(environ, "abc") {
		t.Fatalf("an argument was read as the environment: %q", environ)
	}
	if _, ok := procargsEnvironment(procargs([]string{"tool"}, "")[:6]); ok {
		t.Fatal("a truncated buffer was parsed")
	}
}

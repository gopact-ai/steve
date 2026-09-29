package procgroup

import (
	"fmt"
	"testing"
)

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

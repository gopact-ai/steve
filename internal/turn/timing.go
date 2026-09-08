package turn

import (
	"fmt"
	"strings"
	"time"
)

// turnClock times the platform's own share of a turn — everything around
// the agent's thinking — so a slow preparation or landing shows in the log
// with its parts, not as one opaque number. A nil clock records nothing.
type turnClock struct {
	start, last time.Time
	marks       []string
}

func newTurnClock() *turnClock {
	now := time.Now()
	return &turnClock{start: now, last: now}
}

// mark names the time spent since the previous mark.
func (t *turnClock) mark(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.marks = append(t.marks, fmt.Sprintf("%s=%dms", name, now.Sub(t.last).Milliseconds()))
	t.last = now
}

func (t *turnClock) String() string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("%s total=%dms", strings.Join(t.marks, " "), time.Since(t.start).Milliseconds())
}

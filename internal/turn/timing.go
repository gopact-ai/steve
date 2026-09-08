package turn

import (
	"context"
	"log/slog"
	"time"
)

// turnClock times the platform's own share of a turn — everything around
// the agent's thinking — so a slow preparation or landing shows in the log
// with its parts, not as one opaque number. A nil clock records nothing.
type turnClock struct {
	start, last time.Time
	marks       []slog.Attr
}

func newTurnClock() *turnClock {
	now := time.Now()
	return &turnClock{start: now, last: now}
}

// mark names the time spent since the previous mark, in whole milliseconds.
func (t *turnClock) mark(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.marks = append(t.marks, slog.Int64(name, now.Sub(t.last).Milliseconds()))
	t.last = now
}

// attrs is the clock as log fields: the attempt, each mark in the order it
// was taken, and the total, every duration a whole number of milliseconds.
func (t *turnClock) attrs(attemptID string) []slog.Attr {
	if t == nil {
		return []slog.Attr{slog.String("attempt", attemptID)}
	}
	attrs := make([]slog.Attr, 0, len(t.marks)+2)
	attrs = append(attrs, slog.String("attempt", attemptID))
	attrs = append(attrs, t.marks...)
	return append(attrs, slog.Int64("total", time.Since(t.start).Milliseconds()))
}

// report writes the one "turn: timing" line a turn gets.
func (t *turnClock) report(ctx context.Context, attemptID string) {
	slog.LogAttrs(ctx, slog.LevelInfo, "turn: timing", t.attrs(attemptID)...)
}

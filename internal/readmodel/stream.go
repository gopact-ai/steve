package readmodel

import (
	"context"
	"strconv"
	"strings"
)

// EventID is ev's id in the stream: the model's epoch and the event's
// cursor. A client that reconnects names the last id it received.
func (m *Model) EventID(ev Event) string {
	return m.epoch + "." + strconv.FormatUint(ev.Cursor, 10)
}

// SubscribeAfter is Subscribe with the recent events a client reconnecting
// after lastID has not seen, taken under the same lock so that none is
// both replayed and delivered, or neither. An empty lastID, or one from
// another epoch, replays every recent event.
func (m *Model) SubscribeAfter(ctx context.Context, lastID string) ([]Event, <-chan Event, func()) {
	after := m.cursorOf(lastID)
	m.mu.Lock()
	var replay []Event
	for _, ev := range m.recent {
		if ev.Cursor > after {
			replay = append(replay, ev)
		}
	}
	ch, stop := m.subscribeLocked(ctx)
	m.mu.Unlock()
	return replay, ch, stop
}

// cursorOf is the cursor lastID names in this model's epoch, or zero.
func (m *Model) cursorOf(lastID string) uint64 {
	epoch, cursor, ok := strings.Cut(strings.TrimSpace(lastID), ".")
	if !ok || epoch != m.epoch {
		return 0
	}
	n, err := strconv.ParseUint(cursor, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

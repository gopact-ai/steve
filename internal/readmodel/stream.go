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

// Resume is what a client subscribing after the events it names is sent
// before the live stream.
type Resume struct {
	// Reset says the client's events cannot be continued: none of the ids
	// it named is an event of this epoch already sent, or events after it
	// are no longer kept. The client re-reads the state.
	Reset bool
	// Replay are the recent events the client has not seen, or every
	// recent event after a reset.
	Replay []Event
}

// SubscribeAfter is Subscribe with the recent events a client reconnecting
// after lastIDs has not seen, taken under the same lock so that none is
// both replayed and delivered, or neither. The client resumes after the
// latest id of this epoch it names; one naming no id is sent every recent
// event.
func (m *Model) SubscribeAfter(ctx context.Context, lastIDs ...string) (Resume, <-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var resume Resume
	after, named, placed := uint64(0), false, false
	for _, id := range lastIDs {
		if id == "" {
			continue
		}
		named = true
		if cursor, ok := m.cursorOfLocked(id); ok {
			placed = true
			after = max(after, cursor)
		}
	}
	// The events after the client's are all kept when the oldest kept one
	// follows it directly, or when none has been sent since.
	kept := after == m.published || len(m.recent) > 0 && m.recent[0].Cursor <= after+1
	if named && (!placed || !kept) {
		resume.Reset, after = true, 0
	}
	for _, ev := range m.recent {
		if ev.Cursor > after {
			resume.Replay = append(resume.Replay, ev)
		}
	}
	ch, stop := m.subscribeLocked(ctx)
	return resume, ch, stop
}

// cursorOfLocked is the cursor id names, when it is an event of this
// epoch already published.
func (m *Model) cursorOfLocked(id string) (uint64, bool) {
	epoch, cursor, ok := strings.Cut(strings.TrimSpace(id), ".")
	if !ok || epoch != m.epoch {
		return 0, false
	}
	n, err := strconv.ParseUint(cursor, 10, 64)
	if err != nil || n > m.published {
		return 0, false
	}
	return n, true
}

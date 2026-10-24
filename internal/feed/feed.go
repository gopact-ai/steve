// Package feed is the one ordered stream of product changes a client follows:
// messages, task cards and approvals. Each event carries a sequence number; a
// client that reconnects resumes after the last one it saw. The feed keeps a
// bounded window in memory, so a client that fell further behind — or one that
// reconnects after a restart — is told its cursor is too old and reloads the
// snapshot instead of receiving a partial history.
package feed

import (
	"encoding/json"
	"errors"
	"sync"
)

// ErrCursorTooOld means the events after the cursor are no longer held; the
// client must reload the snapshot and follow from the snapshot's cursor.
var ErrCursorTooOld = errors.New("feed: cursor too old")

// Event is one change. Type names it for the client (message, task.updated,
// approval.opened, ...); Data is the full current value of what changed, so a
// client never has to merge partial updates.
type Event struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Feed is safe for concurrent use. The zero value is not usable; call New.
type Feed struct {
	mu     sync.Mutex
	epoch  string
	next   uint64
	window []Event
	limit  int
	subs   map[chan struct{}]struct{}
}

// New returns a feed holding the last limit events. Epoch distinguishes this
// process's sequence from a previous one's: a cursor from another epoch is
// always too old.
func New(epoch string, limit int) *Feed {
	if limit <= 0 {
		limit = 1024
	}
	return &Feed{epoch: epoch, next: 1, limit: limit, subs: map[chan struct{}]struct{}{}}
}

// Cursor is an opaque position: the epoch and the last sequence included.
type Cursor struct {
	Epoch string `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

// Head is the cursor of the newest event; a snapshot taken under the same
// lock as the state it reflects pairs with it.
func (f *Feed) Head() Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Cursor{Epoch: f.epoch, Seq: f.next - 1}
}

// Publish appends an event and wakes subscribers. The value is encoded now,
// so later changes to it do not alter what was published.
func (f *Feed) Publish(kind string, value any) (Event, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Event{}, err
	}
	f.mu.Lock()
	event := Event{Seq: f.next, Type: kind, Data: raw}
	f.next++
	f.window = append(f.window, event)
	if len(f.window) > f.limit {
		f.window = append([]Event(nil), f.window[len(f.window)-f.limit:]...)
	}
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
	return event, nil
}

// After returns the events after the cursor, oldest first. A cursor from
// another epoch, or one older than the window, is ErrCursorTooOld; a cursor
// ahead of the feed is treated as too old as well, since it cannot be ours.
func (f *Feed) After(c Cursor) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Epoch != f.epoch || c.Seq >= f.next {
		if c.Epoch == f.epoch && c.Seq == f.next-1 {
			return nil, nil
		}
		return nil, ErrCursorTooOld
	}
	if c.Seq == f.next-1 {
		return nil, nil
	}
	if len(f.window) == 0 || c.Seq+1 < f.window[0].Seq {
		return nil, ErrCursorTooOld
	}
	start := int(c.Seq + 1 - f.window[0].Seq)
	return append([]Event(nil), f.window[start:]...), nil
}

// Subscribe returns a channel that receives a signal whenever an event is
// published, and a func to stop. Signals coalesce: read with After.
func (f *Feed) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

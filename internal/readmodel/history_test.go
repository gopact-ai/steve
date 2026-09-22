package readmodel

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func historyFixture(t *testing.T, hours ...int) (*Model, *ledger.Ledger) {
	t.Helper()
	var now time.Time
	book, err := ledger.Open(t.TempDir(), ledger.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	for i, hour := range hours {
		now = historyTime(hour)
		if _, err := book.Begin(t.Context(), fmt.Sprintf("event-%d", i), "test", "open", "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	return New(Sources{Ledger: Ledger{Book: book}}), book
}

func historyTime(hour int) time.Time { return time.Date(2026, 9, 19, hour, 0, 0, 0, time.UTC) }

func TestHistoryInterleavedLimitOne(t *testing.T) {
	m, _ := historyFixture(t, 8, 10)
	m.observations = []Observation{{At: historyTime(9), Kind: "node.up", Subject: "at-nine"}}
	cursor := ""
	var subjects []string
	for page := 0; page < 6; page++ {
		entries, next, err := m.History(t.Context(), cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 1 {
			t.Fatalf("page %d exceeded limit: %+v", page, entries)
		}
		for _, entry := range entries {
			subjects = append(subjects, entry.Subject)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{"event-1", "at-nine", "event-0"}; !reflect.DeepEqual(subjects, want) {
		t.Fatalf("paged subjects = %v, want %v", subjects, want)
	}
}

func TestHistoryObservationOnlyPageIsBounded(t *testing.T) {
	m := New(Sources{})
	for i := range 5 {
		m.observations = append(m.observations, Observation{At: historyTime(i), Kind: "node.up", Subject: fmt.Sprint(i)})
	}
	entries, _, err := m.History(t.Context(), "", 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("limit=1 returned %d entries, err=%v", len(entries), err)
	}
}

func TestHistoryOrdersLedgerByTimeNotSequence(t *testing.T) {
	m, _ := historyFixture(t, 10, 8)
	entries, _, err := m.History(t.Context(), "", 1)
	if err != nil || len(entries) != 1 || entries[0].Subject != "event-0" {
		t.Fatalf("first chronological page = %+v, err=%v", entries, err)
	}
}

func TestHistoryEqualTimestampDoesNotDropObservations(t *testing.T) {
	m, _ := historyFixture(t, 10)
	m.observations = []Observation{{At: historyTime(10), Kind: "node.up", Subject: "same-time"}}
	entries, _, err := m.History(t.Context(), "", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("equal-time history = %+v, err=%v", entries, err)
	}
}

func historyWalk(t *testing.T, m *Model, limit int) []HistoryEntry {
	t.Helper()
	var out []HistoryEntry
	cursor := ""
	cursors := map[string]bool{}
	for page := 0; page < 100; page++ {
		entries, next, err := m.History(t.Context(), cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		repeated, repeatedNext, err := m.History(t.Context(), cursor, limit)
		if err != nil || !reflect.DeepEqual(repeated, entries) || repeatedNext != next {
			t.Fatalf("repeated page differs: cursor=%q err=%v", cursor, err)
		}
		if len(entries) > limit {
			t.Fatalf("page contains %d entries, limit=%d", len(entries), limit)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		if len(entries) != limit || cursors[next] {
			t.Fatalf("nonterminal page did not progress: entries=%d next=%q", len(entries), next)
		}
		cursors[next], cursor = true, next
	}
	t.Fatal("history did not terminate")
	return nil
}

func TestHistoryEqualTimestampsAndIdenticalObservations(t *testing.T) {
	m, _ := historyFixture(t, 10, 10, 10)
	for range 4 {
		m.observations = append(m.observations, Observation{At: historyTime(10), Kind: "node.up", Subject: "identical"})
	}
	want, next, err := m.History(t.Context(), "", 20)
	if err != nil || next != "" || len(want) != 7 {
		t.Fatalf("full history=%+v next=%q err=%v", want, next, err)
	}
	for _, limit := range []int{1, 2, 3, 4, 7, 8} {
		if got := historyWalk(t, m, limit); !reflect.DeepEqual(got, want) {
			t.Fatalf("limit=%d lost/repeated equal-time entries: %+v", limit, got)
		}
	}
}

func TestHistoryEmptyAndObservationOnlyPages(t *testing.T) {
	for _, withLedger := range []bool{false, true} {
		m := New(Sources{})
		if withLedger {
			m, _ = historyFixture(t)
		}
		entries, next, err := m.History(t.Context(), "", 1)
		if err != nil || len(entries) != 0 || next != "" {
			t.Fatalf("empty history=%+v next=%q err=%v", entries, next, err)
		}
		for _, hour := range []int{12, 4, 8} {
			m.observations = append(m.observations, Observation{At: historyTime(hour), Kind: "node.up", Subject: fmt.Sprint(hour)})
		}
		got := historyWalk(t, m, 1)
		if len(got) != 3 || got[0].Subject != "12" || got[1].Subject != "8" || got[2].Subject != "4" {
			t.Fatalf("observation-only pages=%+v", got)
		}
	}
}

func TestHistoryCursorPinsBothSourcesAcrossAppendsAndReload(t *testing.T) {
	m, book := historyFixture(t, 10, 8)
	m.observations = []Observation{{At: historyTime(9), Kind: "node.up", Subject: "nine"}, {At: historyTime(7), Kind: "node.up", Subject: "seven"}}
	_, cursor, err := m.History(t.Context(), "", 1)
	if err != nil || cursor == "" {
		t.Fatal(cursor, err)
	}
	want, wantNext, err := m.History(t.Context(), cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Backdated ledger writes and equal-time observations cannot enter an
	// existing traversal, nor can a reload reassign identical observations.
	if _, err := book.Begin(t.Context(), "late", "test", "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	m.observations = append(m.observations, Observation{At: historyTime(9), Kind: "node.up", Subject: "new-nine"})
	restored := New(m.src)
	restored.observations = append([]Observation(nil), m.observations...)
	got, next, err := restored.History(t.Context(), cursor, 2)
	if err != nil || !reflect.DeepEqual(got, want) || next != wantNext {
		t.Fatalf("snapshot changed: got=%+v next=%q err=%v want=%+v next=%q", got, next, err, want, wantNext)
	}
	restored.observations = restored.observations[1:]
	if _, _, err := restored.History(t.Context(), cursor, 2); !errors.Is(err, ErrHistoryCursorExpired) {
		t.Fatalf("evicted observation cursor did not expire: %v", err)
	}
}

func TestHistoryInvalidCursor(t *testing.T) {
	m := New(Sources{})
	for _, cursor := range []string{"1", "12345", "not-a-cursor!", "e30", "bnVsbA", strings.Repeat("a", 2048)} {
		if _, _, err := m.History(t.Context(), cursor, 1); !errors.Is(err, ErrHistoryCursor) {
			t.Errorf("cursor %q: %v", cursor, err)
		}
	}
}

func TestHistoryCursorRejectsMalformedPayloads(t *testing.T) {
	m, _ := historyFixture(t, 8, 10)
	_, cursor, err := m.History(t.Context(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := decodeHistoryCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*historyCursor){
		func(c *historyCursor) { c.Version++ },
		func(c *historyCursor) { c.Ledger = -1 },
		func(c *historyCursor) { c.Before.Source = 2 },
		func(c *historyCursor) { c.Before.ID = 0 },
		func(c *historyCursor) { c.Before.ID = c.Ledger + 1 },
		func(c *historyCursor) { c.Before.Source = historyObservation; c.Before.ID = 1 },
		func(c *historyCursor) { c.Observations = -1 },
		func(c *historyCursor) { c.Observations = observationsKept + 1 },
		func(c *historyCursor) { c.Digest = "not-a-digest" },
	} {
		malformed := valid
		mutate(&malformed)
		if _, _, err := m.History(t.Context(), encodeHistoryCursor(malformed), 1); !errors.Is(err, ErrHistoryCursor) {
			t.Errorf("malformed cursor %+v: %v", malformed, err)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	for _, payload := range []string{string(raw) + "{}", strings.TrimSuffix(string(raw), "}") + `,"extra":1}`, strings.TrimSuffix(string(raw), "}") + `,"v":1}`} {
		if _, _, err := m.History(t.Context(), base64.RawURLEncoding.EncodeToString([]byte(payload)), 1); !errors.Is(err, ErrHistoryCursor) {
			t.Errorf("accepted noncanonical cursor %s: %v", payload, err)
		}
	}
}

func TestHistoryBoundedMergeMatchesWholeTraversal(t *testing.T) {
	m, _ := historyFixture(t, 8, 10, 5, 7, 10, 5)
	for i, hour := range []int{3, 12, 6, 7, 1, 10, 12, 5, 5} {
		m.observations = append(m.observations, Observation{At: historyTime(hour).Add(time.Duration(i%3) * time.Nanosecond), Kind: "node.up", Subject: fmt.Sprintf("observation-%d", i)})
	}
	want, next, err := m.History(t.Context(), "", 200)
	if err != nil || len(want) != 15 || next != "" {
		t.Fatal(len(want), next, err)
	}
	for i := 1; i < len(want); i++ {
		if want[i].At.After(want[i-1].At) {
			t.Fatal("nonchronological full history")
		}
	}
	for _, limit := range []int{1, 2, 3, 4, 7, 10, 15, 200} {
		if got := historyWalk(t, m, limit); !reflect.DeepEqual(got, want) {
			t.Fatalf("limit=%d does not match full traversal: %+v", limit, got)
		}
	}
}

func TestHistoryBoundsDefaultsAndRetainedObservations(t *testing.T) {
	m := New(Sources{})
	for i := range observationsKept + 10 {
		m.observations = append(m.observations, Observation{At: historyTime(1).Add(time.Duration(i) * time.Nanosecond), Kind: "node.up", Subject: fmt.Sprint(i)})
	}
	for _, limit := range []int{-1, 0, 1, 60, 200, 201} {
		want := limit
		if limit <= 0 || limit > 200 {
			want = 60
		}
		entries, next, err := m.History(t.Context(), "", limit)
		if err != nil || len(entries) != want || next == "" {
			t.Fatalf("limit=%d entries=%d next=%q err=%v", limit, len(entries), next, err)
		}
	}
	if got := historyWalk(t, m, 200); len(got) != observationsKept || got[len(got)-1].Subject != "10" {
		t.Fatalf("history did not use the retained tail: %d entries", len(got))
	}
}

func TestHistoryLedgerFailureDoesNotPublishPartialCursor(t *testing.T) {
	m, book := historyFixture(t, 8, 10)
	m.observations = []Observation{{At: historyTime(9), Kind: "node.up", Subject: "nine"}}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	entries, next, err := m.History(t.Context(), "", 1)
	if err == nil || len(entries) != 0 || next != "" {
		t.Fatalf("source failure advanced cursor: entries=%+v next=%q err=%v", entries, next, err)
	}
}

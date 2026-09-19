package ledger

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func historyBook(t *testing.T) *Ledger {
	t.Helper()
	book, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

func TestHistoryEventsOrderNanosecondsAndNonmonotonicClock(t *testing.T) {
	book := historyBook(t)
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.FixedZone("offset", 8*3600))
	offsets := []time.Duration{0, time.Nanosecond, time.Second, 10 * time.Nanosecond, 100 * time.Millisecond, 100 * time.Millisecond}
	for i, offset := range offsets {
		book.now = func() time.Time { return base.Add(offset) }
		if _, err := book.Begin(t.Context(), fmt.Sprint(i), "test", "open", "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	var before *EventPosition
	var fence *int64
	var got []int64
	for range 7 {
		events, through, err := book.HistoryEvents(t.Context(), before, fence, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			break
		}
		if len(events) != 1 {
			t.Fatalf("unbounded page: %d", len(events))
		}
		got = append(got, events[0].Seq)
		before, fence = &EventPosition{At: events[0].At, Seq: events[0].Seq}, &through
	}
	if want := []int64{3, 6, 5, 4, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want=%v", got, want)
	}
}

func TestHistoryEventsFenceIncludingEmptyLedger(t *testing.T) {
	for _, initiallyEmpty := range []bool{true, false} {
		book := historyBook(t)
		if !initiallyEmpty {
			if _, err := book.Begin(t.Context(), "old", "test", "open", "test", nil); err != nil {
				t.Fatal(err)
			}
		}
		before, fence, err := book.HistoryEvents(t.Context(), nil, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		book.now = func() time.Time { return time.Unix(0, 1) }
		if _, err := book.Begin(t.Context(), "backdated", "test", "open", "test", nil); err != nil {
			t.Fatal(err)
		}
		got, _, err := book.HistoryEvents(t.Context(), nil, &fence, 10)
		if err != nil || !reflect.DeepEqual(got, before) {
			t.Fatalf("fence=%d before=%+v got=%+v err=%v", fence, before, got, err)
		}
	}
}

func TestHistoryEventsSeeksIndexWithoutReadingOtherPayloads(t *testing.T) {
	book := historyBook(t)
	// One bulk fixture keeps scale independent of fsync latency. Corrupt
	// old payloads must not be decoded by the recent page.
	if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS (VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<10000)
      INSERT INTO events(seq, operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at)
      SELECT seq, 'test', 1, 1, '', 'open', 'test', CASE WHEN seq=1 THEN 'bad-json' ELSE '[]' END, 'null',
      '2026-09-19T10:00:00.' || rtrim(printf('%09d', seq), '0') || 'Z' FROM n`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 19, 10, 0, 0, 5000, time.UTC)
	position := &EventPosition{At: at, Seq: 5000}
	for _, boundary := range []string{historySameTime, historyOlder} {
		args := []any{10000, strings.TrimSuffix(at.Format(time.RFC3339Nano), "Z")}
		if boundary == historySameTime {
			args = append(args, 5000)
		}
		args = append(args, 2)
		rows, err := book.db.Query(`EXPLAIN QUERY PLAN `+historyEventQuery(boundary), args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		explain := strings.Join(plan, "\n")
		t.Log(explain)
		if !strings.Contains(explain, "SEARCH events USING INDEX events_history_time_seq") || strings.Contains(explain, "TEMP B-TREE") {
			t.Fatalf("history page must seek instead of scan/sort:\n%s", explain)
		}
	}
	events, _, err := book.HistoryEvents(t.Context(), position, nil, 2)
	if err != nil || len(events) != 2 || events[0].Seq != 4999 || events[1].Seq != 4998 {
		t.Fatalf("bounded indexed page=%+v err=%v", events, err)
	}
	book.now = func() time.Time { return time.Now().UTC() }
	if _, err := book.Begin(t.Context(), "after-index", "test", "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := book.HistoryEvents(t.Context(), nil, nil, 1)
	if err != nil || len(got) != 1 || got[0].OperationID != "after-index" {
		t.Fatalf("index did not include later writes: %+v %v", got, err)
	}
}

func TestHistoryEventsReadDoesNotMutateReplicaPosition(t *testing.T) {
	book := historyBook(t)
	if err := validateReplicaSchema(book.db); err != nil {
		t.Fatalf("history index incompatible with replicated schema: %v", err)
	}
	before, err := book.ReplicaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := book.HistoryEvents(t.Context(), nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	after, err := book.ReplicaVersion()
	if err != nil || before != after {
		t.Fatalf("history read changed replica version: %d -> %d, %v", before, after, err)
	}
	for _, limit := range []int{-1, 0, 202} {
		if _, _, err := book.HistoryEvents(t.Context(), nil, nil, limit); err == nil {
			t.Fatalf("accepted limit=%d", limit)
		}
	}
}

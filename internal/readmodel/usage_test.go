package readmodel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func usageRecord(start time.Time, agent, model string, reported bool, input, output int64) attempt.Record {
	return attempt.Record{Spec: attempt.Spec{Agent: agent}, State: attempt.Bound, StartedAt: start, EndedAt: start.Add(time.Minute),
		Usage: &attempt.Usage{Model: model, Reported: reported, Input: input, Output: output}}
}

func TestUsagePeriodsShareCalendarWindowsAndTotals(t *testing.T) {
	loc := time.FixedZone("hub", 8*60*60)
	now := time.Date(2026, 9, 6, 10, 30, 0, 0, loc)
	today := time.Date(2026, 9, 6, 0, 0, 0, 0, loc)
	starts := []time.Time{
		today.AddDate(0, 0, -30), // outside every period
		today.AddDate(0, 0, -29), // first instant of 30d
		today.AddDate(0, 0, -6).Add(-time.Nanosecond),
		today.AddDate(0, 0, -6), // first instant of 7d
		today.Add(-time.Nanosecond),
		today.UTC(), // stored UTC, grouped by the hub's local day
		now,
		now.Add(time.Nanosecond), // not yet observed
		{},                       // unknown time stays outside calendar windows
	}
	records := make([]attempt.Record, 0, len(starts))
	for _, start := range starts {
		records = append(records, usageRecord(start, "agent", "model", true, 1, 2))
	}
	u := usage(records, now)
	if u.Timezone != "hub" || u.Total.Attempts != len(starts) {
		t.Fatalf("cumulative usage = %+v", u)
	}
	for _, tc := range []struct {
		key      string
		days     int
		buckets  int
		attempts int
		interval string
	}{{"1d", 1, 11, 2, "hour"}, {"7d", 7, 7, 4, "day"}, {"30d", 30, 30, 6, "day"}} {
		t.Run(tc.key, func(t *testing.T) {
			p := u.Periods[tc.key]
			if !p.From.Equal(today.AddDate(0, 0, 1-tc.days)) || !p.To.Equal(now) || p.Interval != tc.interval || len(p.Series) != tc.buckets {
				t.Fatalf("window = %+v", p)
			}
			if p.Total.Attempts != tc.attempts || p.Total.Tokens.Total != int64(3*tc.attempts) {
				t.Fatalf("window total = %+v", p.Total)
			}
			assertUsageSum(t, p.Total, p.Series)
			assertUsageSum(t, p.Total, p.ByAgent)
			assertUsageSum(t, p.Total, p.ByModel)
		})
	}
	day := u.Periods["1d"]
	if day.Series[0].Key != "2026-09-06T00:00:00+08:00" || day.Series[1].Attempts != 0 || day.Series[10].Key != "2026-09-06T10:00:00+08:00" {
		t.Fatalf("hour buckets = %+v", day.Series)
	}
}

func TestUsageKeepsUnreportedAndUnknownDimensions(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	reported := usageRecord(start, "same", "same", true, 10, 5)
	reported.Usage.CachedRead, reported.Usage.CachedWrite, reported.Usage.Context = 100, 200, 300
	contextOnly := usageRecord(start, "same", "", false, 0, 0)
	contextOnly.Usage.Context = 400
	missing := usageRecord(start, "", "", false, 0, 0)
	missing.Usage = nil
	zero := usageRecord(start, "same", "same", true, 0, 0)
	zero.EndedAt = start.Add(-time.Second) // malformed duration must not reduce totals
	u := usage([]attempt.Record{reported, contextOnly, missing, zero}, now)
	want := UsageRow{Key: "total", Attempts: 4, Unreported: 2, Seconds: 180,
		Tokens: Tokens{Input: 10, Output: 5, Total: 15, CachedRead: 100, CachedWrite: 200, Context: 700}}
	if u.Total != want {
		t.Fatalf("total = %+v, want %+v", u.Total, want)
	}
	assertUsageSum(t, u.Total, u.ByDay)
	assertUsageSum(t, u.Total, u.ByAgent)
	assertUsageSum(t, u.Total, u.ByModel)
	if len(u.ByAgent) != 2 || u.ByAgent[0].Key != "" || u.ByAgent[1].Key != "same" || len(u.ByModel) != 2 || u.ByModel[0].Attempts != 2 {
		t.Fatalf("missing or colliding dimensions: agents=%+v models=%+v", u.ByAgent, u.ByModel)
	}
	for _, p := range u.Periods {
		if p.Total != want {
			t.Fatalf("period total differs: %+v", p.Total)
		}
		assertUsageSum(t, p.Total, p.Series)
		assertUsageSum(t, p.Total, p.ByAgent)
		assertUsageSum(t, p.Total, p.ByModel)
	}
}

func TestUsageHourlyBucketsFollowDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		month time.Month
		day   int
		hours int
	}{{"spring", time.March, 8, 23}, {"fall", time.November, 1, 25}} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, tc.month, tc.day, 0, 0, 0, 0, loc)
			now := start.AddDate(0, 0, 1).Add(-time.Second)
			var records []attempt.Record
			for at := start; at.Before(now); at = at.Add(time.Hour) {
				records = append(records, usageRecord(at, "agent", "model", true, 1, 0))
			}
			u := usage(records, now)
			p := u.Periods["1d"]
			if len(p.Series) != tc.hours || p.Total.Attempts != tc.hours {
				t.Fatalf("%s has %d buckets and %d attempts, want %d", tc.name, len(p.Series), p.Total.Attempts, tc.hours)
			}
			seen := map[string]bool{}
			for _, row := range p.Series {
				if seen[row.Key] || row.Attempts != 1 {
					t.Fatalf("hour was merged or lost: %+v", row)
				}
				seen[row.Key] = true
			}
			if !u.Periods["7d"].From.Equal(start.AddDate(0, 0, -6)) {
				t.Fatal("calendar window drifted across DST")
			}
		})
	}
}

func TestUsageSnapshotCountsClosedLedgerAttemptsOnce(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	attempts := attempt.New(book)
	for _, id := range []string{"closed", "running"} {
		if _, err := attempts.Open(t.Context(), attempt.Spec{ID: id, Agent: "agent", Scope: attempt.ScopeNone}); err != nil {
			t.Fatal(err)
		}
		for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
			if _, err := attempts.Advance(t.Context(), id, state, "test", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := attempts.FailWith(t.Context(), "closed", "test", "failed after spending", &attempt.Usage{Reported: true, Input: 3, Output: 4}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := tasks.Create(task.Task{Goal: "cached duplicate", Channel: "test", Attempts: []task.Attempt{{StartedAt: now, EndedAt: now, Tokens: task.Tokens{Input: 3, Output: 4, Total: 7}}}}); err != nil {
		t.Fatal(err)
	}
	snap := New(Sources{Tasks: tasks, Ledger: Ledger{Book: book, Attempts: attempts}}).Snapshot(t.Context())
	if len(snap.Attempts) != 1 || snap.Usage.Total.Attempts != 1 || snap.Usage.Total.Tokens.Total != 7 || snap.Usage.Periods["1d"].Total.Attempts != 1 {
		t.Fatalf("running or cached attempt affected usage: %+v", snap.Usage)
	}
}

func TestUsageEmptySnapshotStillHasCurrentCalendarBuckets(t *testing.T) {
	snap := New(Sources{}).Snapshot(t.Context())
	if len(snap.Usage.Periods) != 3 || snap.Usage.Timezone == "" {
		t.Fatalf("missing empty dashboard: %+v", snap.Usage)
	}
	for _, p := range snap.Usage.Periods {
		if len(p.Series) == 0 || p.ByAgent == nil || p.ByModel == nil || p.Total.Attempts != 0 || !p.To.Equal(snap.At) {
			t.Fatalf("empty period = %+v", p)
		}
		for _, row := range p.Series {
			if row.Attempts != 0 || row.Unreported != 0 || row.Tokens.Total != 0 {
				t.Fatalf("empty bucket fabricated usage: %+v", row)
			}
		}
	}
}

func assertUsageSum(t *testing.T, want UsageRow, rows []UsageRow) {
	t.Helper()
	var got UsageRow
	for _, row := range rows {
		got.Tokens = got.Tokens.add(row.Tokens)
		got.Attempts += row.Attempts
		got.Seconds += row.Seconds
		got.Unreported += row.Unreported
	}
	got.Key = want.Key
	if got != want {
		t.Fatalf("rows sum to %+v, want %+v", got, want)
	}
}

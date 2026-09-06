package readmodel

import (
	"math"
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

func TestUsageTaskTreesUseIntervalUnionsAndKeepAllDimensions(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	tasks := []Task{
		{ID: "root", Title: "Scheduled work", Origin: "schedule:42", ProjectID: "p"},
		{ID: "child", Parent: "root"}, {ID: "grandchild", Parent: "child"},
		{ID: "other", Goal: "Chat work", ProjectID: "q"},
		{ID: "orphan", Parent: "missing"}, {ID: "cycle-a", Parent: "cycle-b"}, {ID: "cycle-b", Parent: "cycle-a"},
	}
	record := func(taskID, agent, harness, projectID string, startMinutes, endMinutes int, tokens int64) attempt.Record {
		r := usageRecord(day.Add(time.Duration(startMinutes)*time.Minute), agent, "model", true, tokens, 0)
		r.TaskID, r.Harness, r.Project = taskID, harness, projectID
		r.EndedAt = day.Add(time.Duration(endMinutes) * time.Minute)
		return r
	}
	missing := record("orphan", "a", "h2", "", 510, 510, 40)
	missing.EndedAt = time.Time{}
	records := []attempt.Record{
		record("root", "a", "h1", "p", 480, 490, 600),
		record("child", "b", "h2", "p", 485, 495, 600),
		record("grandchild", "b", "h2", "p", 487, 488, 60),
		record("child", "b", "h2", "p", 540, 545, 300),
		record("other", "a", "h1", "q", 500, 502, 120), missing,
		record("unknown-task", "a", "h1", "", 600, 601, 60),
		record("", "a", "h1", "service", 600, 601, 30),
		record("cycle-a", "a", "h1", "", 660, 661, 60),
	}
	p := usage(records, now, tasks).Periods["1d"]
	want := TaskDurationStats{Count: 5, Measured: 4, MinSeconds: 60, MaxSeconds: 1200, AverageSeconds: 360, TotalSeconds: 1440}
	if p.Tasks != want {
		t.Fatalf("root duration statistics=%+v want=%+v", p.Tasks, want)
	}
	var root TaskUsageRow
	for _, row := range p.ByTask {
		if row.TaskID == "root" {
			root = row
		}
		if row.TaskID == "orphan" || row.TaskID == "unknown-task" || row.TaskID == "cycle-a" {
			if row.Trigger != "" || row.Title != "" {
				t.Fatalf("missing root metadata fabricated: %+v", row)
			}
		}
	}
	if root.Attempts != 4 || root.Seconds != 1200 || root.ElapsedSeconds != 1200 || root.Tokens.Total != 1560 || root.Title != "Scheduled work" || root.Trigger != "schedule" || root.Agent != "a · b" || root.Harness != "h1 · h2" {
		t.Fatalf("root row=%+v", root)
	}
	for _, groups := range [][]UsageRow{p.ByAgent, p.ByModel, p.ByHarness, p.ByTrigger, p.ByProject} {
		assertUsageSum(t, p.Total, groups)
	}
	if got := usageGroupRow(t, p.ByHarness, "h2"); got.Tasks == nil || got.Tasks.Count != 2 || got.Tasks.Measured != 1 || got.Tasks.TotalSeconds != 900 {
		t.Fatalf("harness duration used full unrelated root work: %+v", got)
	}
	if got := usageGroupRow(t, p.ByTrigger, "schedule"); got.Attempts != 4 || got.Tasks.Count != 1 || got.Tasks.TotalSeconds != 1200 {
		t.Fatalf("child work lost root trigger: %+v", got)
	}
	if p.Total.Tokens.Total != 1870 || p.Throughput.UnmeasuredTokens != 40 {
		t.Fatalf("totals=%+v throughput=%+v", p.Total, p.Throughput)
	}
	closeTo(t, p.Throughput.ActiveTPM, 1830.0/24)
	closeTo(t, p.Throughput.WindowTPM, 1830.0/720)
	closeTo(t, p.Throughput.PeakTPM, 180)
}

func TestUsageThroughputUniformlyAllocatesAcrossPartialMinutesAndBuckets(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	a := usageRecord(day.Add(30*time.Second), "a", "model", true, 120, 0)
	a.EndedAt = day.Add(150 * time.Second)
	b := usageRecord(day.Add(time.Minute), "b", "model", true, 60, 0)
	b.EndedAt = day.Add(2 * time.Minute)
	p := usage([]attempt.Record{a, b}, day.Add(3*time.Minute), nil).Periods["1d"]
	closeTo(t, p.Throughput.WindowTPM, 60)
	closeTo(t, p.Throughput.ActiveTPM, 90)
	closeTo(t, p.Throughput.PeakTPM, 120)
	if !p.Throughput.Estimated || p.Throughput.MeasuredTokens != 180 || p.Tasks.Count != 0 {
		t.Fatalf("throughput claimed exact generation speed or invented tasks: %+v", p)
	}
	a.StartedAt, a.EndedAt = day.Add(30*time.Minute), day.Add(150*time.Minute)
	p = usage([]attempt.Record{a}, day.Add(150*time.Minute), nil).Periods["1d"]
	for i, want := range []float64{.5, 1, 1} {
		closeTo(t, p.Series[i].TPM, want)
	}
	closeTo(t, p.Throughput.WindowTPM, .8)
	closeTo(t, p.Throughput.ActiveTPM, 1)
	closeTo(t, p.Throughput.PeakTPM, 1)
	closeTo(t, p.Series[0].TPM*60+p.Series[1].TPM*60+p.Series[2].TPM*30, 120)
}

func TestUsageUnreportedTokensAreZeroButCoverageAndContextRemain(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r := usageRecord(now.Add(-time.Minute), "a", "model", false, 120, 30)
	r.Usage.CachedRead, r.Usage.CachedWrite, r.Usage.Context = 100, 20, 500
	r.TaskID = "missing"
	p := usage([]attempt.Record{r}, now, nil).Periods["1d"]
	if p.Total.Tokens != (Tokens{Context: 500}) || p.Total.Unreported != 1 || p.Throughput.WindowTPM != 0 || p.Throughput.PeakTPM != 0 || p.Tasks.Measured != 1 || p.Tasks.TotalSeconds != 60 {
		t.Fatalf("missing token report fabricated spend or lost duration: %+v", p)
	}
	for _, row := range p.Series {
		if row.Tokens.Total != 0 || row.TPM != 0 {
			t.Fatalf("missing report became a plotted token value: %+v", row)
		}
	}
}

func TestUsageRangesKeepTaskAndThroughputCohortsConsistent(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	a := usageRecord(day.Add(-30*time.Minute), "a", "m", true, 120, 0)
	a.EndedAt = day.Add(30 * time.Minute)
	a.TaskID = "root"
	b := usageRecord(day.Add(30*time.Minute), "a", "m", true, 120, 0)
	b.EndedAt = day.Add(90 * time.Minute)
	b.TaskID = "child"
	u := usage([]attempt.Record{a, b}, day.Add(2*time.Hour), []Task{{ID: "root", Origin: "plan"}, {ID: "child", Parent: "root"}})
	for _, tc := range []struct {
		key     string
		tokens  int64
		seconds float64
	}{{"1d", 120, 3600}, {"7d", 240, 7200}, {"30d", 240, 7200}} {
		p := u.Periods[tc.key]
		if p.Total.Tokens.Total != tc.tokens || p.Tasks.Count != 1 || p.Tasks.TotalSeconds != tc.seconds || len(p.ByTask) != 1 || p.ByTask[0].Tokens.Total != tc.tokens {
			t.Fatalf("%s mixes task or token cohorts: %+v", tc.key, p)
		}
		closeTo(t, p.Throughput.MeasuredTokens, float64(tc.tokens))
		closeTo(t, p.Throughput.ActiveTPM, 2)
	}
}

func TestUsageThroughputDoesNotAllocateMinuteSizedHistory(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	curve := makeUsageCurve([]rateSpan{{usageSpan: usageSpan{start: start, end: end}, rate: 1}})
	if len(curve.points) != 2 {
		t.Fatalf("timeline expanded per minute: %d", len(curve.points))
	}
	closeTo(t, curve.peak, 60)
	closeTo(t, curve.at(end), end.Sub(start).Seconds())
}

func TestUsageZeroDurationIsMeasuredWithoutInventingThroughput(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	zero := usageRecord(now.Add(-time.Minute), "a", "m", true, 100, 0)
	zero.TaskID = "zero"
	zero.EndedAt = zero.StartedAt
	zero.Project = "known-project"
	missing := zero
	missing.TaskID = "missing"
	missing.EndedAt = time.Time{}
	p := usage([]attempt.Record{zero, missing}, now, nil).Periods["1d"]
	if p.Tasks.Count != 2 || p.Tasks.Measured != 1 || p.Tasks.TotalSeconds != 0 || p.Tasks.AverageSeconds != 0 || p.Throughput.WindowTPM != 0 || p.Throughput.ActiveTPM != 0 || p.Throughput.PeakTPM != 0 || p.Throughput.UnmeasuredTokens != 200 {
		t.Fatalf("zero/missing duration conflated or divided by zero: %+v", p)
	}
	for _, row := range p.ByTask {
		if row.Project != "known-project" {
			t.Fatalf("known execution project lost with missing task metadata: %+v", row)
		}
	}
}

func usageGroupRow(t *testing.T, rows []UsageRow, key string) UsageRow {
	t.Helper()
	for _, row := range rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("missing group %q", key)
	return UsageRow{}
}
func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %g, want %g", got, want)
	}
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
	u := usage(records, now, nil)
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
	u := usage([]attempt.Record{reported, contextOnly, missing, zero}, now, nil)
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
			u := usage(records, now, nil)
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

package readmodel

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
)

func TestUsageMatchesReference(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{
		time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 7, 12, 34, 56, 123, time.FixedZone("hub", 8*60*60)),
		time.Date(2026, 3, 8, 23, 30, 0, 0, newYork),
		time.Date(2026, 11, 1, 23, 30, 0, 0, newYork),
	} {
		t.Run(now.Format(time.RFC3339), func(t *testing.T) {
			assertUsageEquivalent(t, usage(nil, now, nil), referenceUsage(nil, now, nil))
			for seed := int64(0); seed < 12; seed++ {
				t.Run(fmt.Sprint(seed), func(t *testing.T) {
					records, tasks := usageEquivalenceRecords(now, seed)
					before, err := json.Marshal([]any{records, tasks})
					if err != nil {
						t.Fatal(err)
					}
					want := referenceUsage(records, now, tasks)
					assertUsageEquivalent(t, usage(records, now, tasks), want)
					after, err := json.Marshal([]any{records, tasks})
					if err != nil || string(before) != string(after) {
						t.Fatal("usage modified its input records or tasks")
					}
					// A later request must not change a previously returned result,
					// and changed inputs must not be hidden by a snapshot cache.
					saved := usage(records, now, tasks)
					records = append(records, usageRecord(now, "new", "new", true, 17, 13))
					assertUsageEquivalent(t, usage(records, now.Add(time.Hour), tasks), referenceUsage(records, now.Add(time.Hour), tasks))
					assertUsageEquivalent(t, saved, want)
				})
			}
		})
	}
}

func TestUsageConcurrentCallsOwnResults(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	records, tasks := usageEquivalenceRecords(now, 42)
	want := referenceUsage(records, now, tasks)
	results := make(chan Usage, 8)
	for i := 0; i < cap(results); i++ {
		go func() { results <- usage(records, now, tasks) }()
	}
	for i := 0; i < cap(results); i++ {
		assertUsageEquivalent(t, <-results, want)
	}
}

func usageEquivalenceRecords(now time.Time, seed int64) ([]attempt.Record, []Task) {
	tasks := []Task{
		{ID: "root", Title: "Root", Goal: "Not the title", Origin: "schedule:42", ProjectID: "p"},
		{ID: "child", Parent: "root"}, {ID: "grandchild", Parent: "child"},
		{ID: "chat", Goal: "Fallback title", ProjectID: "q"},
		{ID: "orphan", Parent: "missing"},
		{ID: "cycle-a", Parent: "cycle-b"}, {ID: "cycle-b", Parent: "cycle-a"},
		{ID: "self", Parent: "self"}, {ID: "empty-trigger", Origin: ":value"},
	}
	taskIDs := []string{"", "root", "child", "grandchild", "chat", "orphan", "cycle-a", "cycle-b", "self", "missing", "empty-trigger"}
	dimensions := []string{"", "same", "a", "z", "a · z", "模型"}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	starts := []time.Time{
		{}, day.AddDate(0, 0, -30), day.AddDate(0, 0, -29), day.AddDate(0, 0, -6).Add(-time.Nanosecond),
		day.AddDate(0, 0, -6), day.Add(-time.Nanosecond), day, day.UTC(),
		day.Add(30 * time.Second), day.Add(time.Minute), now.Add(-time.Nanosecond), now, now.Add(time.Nanosecond),
	}
	rng := rand.New(rand.NewSource(seed))
	records := make([]attempt.Record, 300)
	for i := range records {
		start := starts[i%len(starts)]
		if i%3 == 0 {
			start = day.Add(time.Duration(rng.Intn(32*86400)-31*86400) * time.Second).Add(time.Duration(rng.Intn(1000)) * time.Millisecond)
		}
		r := usageRecord(start, dimensions[rng.Intn(len(dimensions))], dimensions[rng.Intn(len(dimensions))], i%5 != 0, int64(rng.Intn(10000)), int64(rng.Intn(3000)))
		r.TaskID = taskIDs[i%len(taskIDs)]
		r.Harness = dimensions[rng.Intn(len(dimensions))]
		r.Project = dimensions[rng.Intn(len(dimensions))]
		r.Usage.CachedRead, r.Usage.CachedWrite, r.Usage.Context = int64(i), int64(2*i), int64(3*i)
		switch i % 9 {
		case 0:
			r.EndedAt = time.Time{}
		case 1:
			r.EndedAt = start
		case 2:
			r.EndedAt = start.Add(-time.Second)
		case 3:
			r.EndedAt = now.Add(time.Hour)
		default:
			r.EndedAt = start.Add(time.Duration(rng.Intn(4*3600))*time.Second + 125*time.Millisecond)
		}
		if i%11 == 0 {
			r.Usage = nil
		}
		records[i] = r
	}
	rng.Shuffle(len(records), func(i, j int) { records[i], records[j] = records[j], records[i] })
	return records, tasks
}

// Compare every field, including nil vs empty slices, ordering, metadata and
// integer counts. Only floats allow the existing throughput tests' tolerance:
// map traversal can change the summation order of task duration statistics.
func assertUsageEquivalent(t *testing.T, got, want Usage) {
	t.Helper()
	var compare func(string, reflect.Value, reflect.Value)
	compare = func(path string, a, b reflect.Value) {
		t.Helper()
		if a.Type() == reflect.TypeFor[time.Time]() {
			at, bt := a.Interface().(time.Time), b.Interface().(time.Time)
			if !at.Equal(bt) || at.Location().String() != bt.Location().String() {
				t.Fatalf("%s: got %v, want %v", path, at, bt)
			}
			return
		}
		switch a.Kind() {
		case reflect.Float64:
			x, y := a.Float(), b.Float()
			if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x-y) > 1e-8*math.Max(1, math.Abs(y)) {
				t.Fatalf("%s: got %.17g, want %.17g", path, x, y)
			}
		case reflect.Struct:
			for i := 0; i < a.NumField(); i++ {
				compare(path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
			}
		case reflect.Pointer:
			if a.IsNil() != b.IsNil() {
				t.Fatalf("%s: nil pointer changed", path)
			}
			if !a.IsNil() {
				compare(path, a.Elem(), b.Elem())
			}
		case reflect.Slice, reflect.Map:
			if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
				t.Fatalf("%s: collection shape changed: got len=%d nil=%v, want len=%d nil=%v", path, a.Len(), a.IsNil(), b.Len(), b.IsNil())
			}
			if a.Kind() == reflect.Slice {
				for i := 0; i < a.Len(); i++ {
					compare(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i))
				}
			} else {
				for _, key := range b.MapKeys() {
					value := a.MapIndex(key)
					if !value.IsValid() {
						t.Fatalf("%s: missing key %v", path, key)
					}
					compare(fmt.Sprintf("%s[%v]", path, key), value, b.MapIndex(key))
				}
			}
		default:
			if !reflect.DeepEqual(a.Interface(), b.Interface()) {
				t.Fatalf("%s: got %v, want %v", path, a.Interface(), b.Interface())
			}
		}
	}
	compare("usage", reflect.ValueOf(got), reflect.ValueOf(want))
}

func TestUsageCurveMatchesReference(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		start := time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)
		spans := make([]rateSpan, 300)
		reference := make([]referenceRateSpan, len(spans))
		for i := range spans {
			at := start.Add(time.Duration(rng.Intn(500)) * 250 * time.Millisecond)
			end := at.Add(time.Duration(rng.Intn(600)) * time.Second)
			rate := float64(rng.Intn(1000)) / 7
			spans[i] = rateSpan{usageSpan: usageSpan{start: at, end: end}, rate: rate}
			reference[i] = referenceRateSpan{referenceUsageSpan: referenceUsageSpan{start: at, end: end}, rate: rate}
		}
		got, want := makeUsageCurve(spans), referenceMakeUsageCurve(reference)
		closeTo(t, got.peak, want.peak)
		if len(got.points) != len(want.points) {
			t.Fatalf("seed %d: curve lost event boundaries", seed)
		}
		for i, point := range got.points {
			if !point.at.Equal(want.points[i].at) {
				t.Fatalf("seed %d: curve reordered boundaries", seed)
			}
			if point.tokens != want.points[i].tokens || point.rate != want.points[i].rate {
				t.Fatalf("seed %d: curve changed floating point arithmetic at point %d", seed, i)
			}
		}
		for at := start.Add(-time.Minute); at.Before(start.Add(time.Hour)); at = at.Add(1250 * time.Millisecond) {
			closeTo(t, got.at(at), want.at(at))
		}
	}
}

func TestUsageSummaryAllocationBudget(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	records, tasks := usageBenchmarkRecords(1000, now)
	allocs := testing.AllocsPerRun(3, func() { runtime.KeepAlive(usage(records, now, tasks)) })
	t.Logf("1000-task summary: %.0f allocs/run", allocs)
	// Allow implementation/toolchain variation, but reject per-task copies of
	// every span and singleton dimension set across all four calendar views.
	if allocs > 35000 {
		t.Fatalf("1000-task summary allocated %.0f objects, budget 35000", allocs)
	}
}

func TestUsageCurveAllocationBudget(t *testing.T) {
	start := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	spans := make([]rateSpan, 1024)
	for i := range spans {
		at := start.Add(time.Duration(i) * time.Minute)
		spans[i] = rateSpan{usageSpan: usageSpan{start: at, end: at.Add(30 * time.Second)}, rate: 1}
	}
	allocs := testing.AllocsPerRun(3, func() { runtime.KeepAlive(makeUsageCurve(spans)) })
	t.Logf("1024-span curve: %.0f allocs/run", allocs)
	if allocs > 10 {
		t.Fatalf("curve allocated %.0f objects, budget 10; boundary minutes must not need a map", allocs)
	}
}

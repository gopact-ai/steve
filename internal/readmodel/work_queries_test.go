package readmodel

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

func historyStateFixture(t testing.TB, count int) *Model {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	at := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{}, Meta: map[string]task.Meta{}}
	in.Tasks["root"] = &task.Task{ID: "root", ProjectID: "p", State: task.StateRunning, Transport: "console", Channel: "opaque", CreatedAt: at, UpdatedAt: at}
	in.Tasks["old-ancestor"] = &task.Task{ID: "old-ancestor", Parent: "root", ProjectID: "p", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}, UpdatedAt: at.Add(-time.Hour)}
	in.Tasks["live-child"] = &task.Task{ID: "live-child", Parent: "old-ancestor", ProjectID: "p", State: task.StateRunning, UpdatedAt: at}
	plans := map[string][]plan.Plan{}
	bindings := map[string]string{}
	for i := range count {
		id := fmt.Sprintf("closed-%05d", i)
		in.Tasks[id] = &task.Task{ID: id, ProjectID: "p", Transport: "console", Channel: "opaque", State: task.StateDone, CreatedAt: at, UpdatedAt: at.Add(time.Duration(i) * time.Second)}
		in.Tasks["root"].Attempts = append(in.Tasks["root"].Attempts, task.Attempt{Member: "agent", Model: "model", StartedAt: at, EndedAt: at.Add(time.Second), Tokens: task.Tokens{Input: 1, Total: 1}})
		plans[id] = []plan.Plan{{ID: id, TaskID: id, ProjectID: "p", Rev: 1, Steps: []plan.Step{{ID: "done", State: plan.StepDone}}}}
		bindings[id] = id
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
		t.Fatal(err)
	}
	store, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"next_id": 1, "plans": plans, "by_task": bindings})
	if err := book.Document("plans").Save(raw); err != nil {
		t.Fatal(err)
	}
	planStore, err := plan.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newSourceFixture()
	fixture.disclosures, fixture.effects = nil, nil
	return New(Sources{Tasks: store, Plans: planStore, Ledger: fixture.adapter()})
}

func TestBaseStateIsLiveClosureAndExplicitRecentCoverage(t *testing.T) {
	read := func(m *Model, count int) (float64, int) {
		snap := m.Snapshot(t.Context())
		ids := map[string]Task{}
		for _, item := range snap.Tasks {
			ids[item.ID] = item
		}
		if len(snap.Tasks) != task.RecentClosedLimit+3 || len(snap.Plans) != task.RecentClosedLimit {
			t.Fatalf("base state includes history instead of closure+recent: tasks=%d plans=%d", len(snap.Tasks), len(snap.Plans))
		}
		if ids["root"].ID == "" || ids["old-ancestor"].ID == "" || ids["live-child"].ID == "" {
			t.Fatal("live ancestor closure was truncated")
		}
		if ids["root"].AttemptCount != count || ids["root"].Tokens.Total != int64(count) || ids["root"].Seconds != int64(count) {
			t.Fatalf("owner summary lost history totals: %+v", ids["root"])
		}
		if snap.TaskCoverage.Total != count+3 || snap.TaskCoverage.Included != len(snap.Tasks) || !snap.TaskCoverage.HasMoreClosed ||
			snap.TaskCoverage.RecentLimit != task.RecentClosedLimit || snap.PlanCoverage.Total != count || !snap.PlanCoverage.HasMore {
			t.Fatalf("snapshot falsely claims complete history: %+v %+v", snap.TaskCoverage, snap.PlanCoverage)
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		return testing.AllocsPerRun(10, func() { _ = m.Snapshot(t.Context()) }), len(raw)
	}
	small, smallBytes := read(historyStateFixture(t, 24), 24)
	large, largeBytes := read(historyStateFixture(t, 10000), 10000)
	t.Logf("24/10k closed task headers + same number plan rows + root accounting rows; selected 23 headers/20 plans: alloc %.0f/%.0f, response bytes %d/%d", small, large, smallBytes, largeBytes)
	if large > small+20 || largeBytes > smallBytes+500 {
		t.Fatal("base state read or output grows with history")
	}
}

func TestHistoricalTaskDetailAndPagesRemainExplicitlyReachable(t *testing.T) {
	m := historyStateFixture(t, 30)
	for _, item := range m.Snapshot(t.Context()).Tasks {
		if item.ID == "closed-00000" {
			t.Fatal("history fixture was not outside the base state")
		}
	}
	detail, err := m.TaskDetail(t.Context(), "closed-00000")
	if err != nil || detail.Task.ID != "closed-00000" || detail.Plan == nil || detail.Plan.ID != "closed-00000" {
		t.Fatalf("historical detail depends on snapshot membership: %+v %v", detail, err)
	}
	q := task.Query{Scope: task.Scope{Kind: "project", ID: "p"}, Status: "closed", Limit: 1}
	seen := map[string]bool{}
	for {
		page, err := m.TaskHistory(t.Context(), q)
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("history page: %+v %v", page, err)
		}
		if seen[page.Items[0].ID] {
			t.Fatal("repeated historical key")
		}
		seen[page.Items[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if !seen["closed-00000"] || len(seen) != 31 {
		t.Fatal("history was silently dropped", len(seen))
	}
	rows, err := m.TaskAccounting("root", "", 1)
	if err != nil || len(rows.Items) != 1 || rows.Total != 30 || rows.NextCursor == "" {
		t.Fatalf("accounting detail missing: %+v %v", rows, err)
	}
}

func TestSnapshotProjectTaskCountsIncludeExcludedHistory(t *testing.T) {
	m := historyStateFixture(t, 24)
	snap := m.Snapshot(t.Context())
	raw, err := json.Marshal(snap.Projects)
	if err != nil {
		t.Fatal(err)
	}
	var projects []struct {
		ID     string       `json:"id"`
		Counts *task.Counts `json:"task_counts"`
	}
	if err := json.Unmarshal(raw, &projects); err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		want := 0
		if p.ID == "p" {
			want = 27
		}
		if p.Counts == nil || p.Counts.Total != want {
			t.Fatalf("project %s: owner counts missing or truncated: %s", p.ID, raw)
		}
	}
}

func TestWorkQueriesDoNotHideSourceFailures(t *testing.T) {
	m := historyStateFixture(t, 24)
	fixture := newSourceFixture()
	fixture.disclosures, fixture.effects = nil, nil
	fixture.fail["live"] = fmt.Errorf("test live source unavailable")
	m.src.Ledger = fixture.adapter()
	if _, err := m.TaskDetail(t.Context(), "closed-00000"); err == nil {
		t.Fatal("detail hid failed live source")
	}
	if _, err := m.TaskHistory(t.Context(), task.Query{Limit: 1}); err == nil {
		t.Fatal("history hid failed live source")
	}
}

func TestHistoricalDetailCompletionUsesOwnerSummaryNotLoadedChildren(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{"root": {ID: "root", ProjectID: "p", State: task.StateRunning}}}
			for i := range 30 {
				id := fmt.Sprintf("child-%02d", i)
				delivery := task.DeliveryDelivered
				if i == 0 && !accepted {
					delivery = task.DeliverySuppressed
				}
				in.Tasks[id] = &task.Task{ID: id, Parent: "root", Origin: "delegate:root", ProjectID: "p", State: task.StateDone, UpdatedAt: time.Unix(int64(i), 0), Result: &task.Result{}, Delivery: &task.Delivery{State: delivery}}
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
				t.Fatal(err)
			}
			store, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			fixture := newSourceFixture()
			fixture.disclosures, fixture.effects = nil, nil
			m := New(Sources{Tasks: store, Ledger: fixture.adapter()})
			detail, err := m.TaskDetail(t.Context(), "root")
			if err != nil {
				t.Fatal(err)
			}
			if detail.Children.Total != 30 || len(detail.Children.Items) != 20 || detail.Task.CanComplete != accepted {
				t.Fatalf("partial child list changed completion eligibility: %+v", detail)
			}
			for _, child := range detail.Children.Items {
				if child.ID == "child-00" {
					t.Fatal("invalid fixture: blocked child must be outside loaded page")
				}
			}
		})
	}
}

// Run with -run '^$' -bench BenchmarkSnapshotFixedLiveHistory -benchtime=1x.
// Setup imports real SQLite records; only the actual Snapshot call is timed.
func BenchmarkSnapshotFixedLiveHistory(b *testing.B) {
	var baselineAlloc float64
	var baselineBytes int
	for _, count := range []int{24, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := historyStateFixture(b, count)
			snap := m.Snapshot(b.Context())
			raw, err := json.Marshal(snap)
			if err != nil {
				b.Fatal(err)
			}
			alloc := testing.AllocsPerRun(5, func() { _ = m.Snapshot(b.Context()) })
			if count == 24 {
				baselineAlloc, baselineBytes = alloc, len(raw)
			}
			if alloc > baselineAlloc+20 || len(raw) > baselineBytes+500 || len(snap.Tasks) != 23 || len(snap.Plans) != 20 {
				b.Fatal("fixed live snapshot grew with history", alloc, len(raw), len(snap.Tasks), len(snap.Plans))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = m.Snapshot(b.Context())
			}
			b.StopTimer()
			b.ReportMetric(float64(len(raw)), "response-B")
			b.ReportMetric(alloc, "allocs/snapshot")
		})
	}
}

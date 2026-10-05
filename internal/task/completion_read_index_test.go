package task

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func suppressionReadStore(t *testing.T, parent State) (*Store, *ledger.Ledger) {
	t.Helper()
	s, book := taskRecordBook(t)
	next := s.clone()
	at := time.Unix(100, 0).UTC()
	next.Tasks["root"] = &Task{ID: "root", State: StateRunning, Channel: "chat", UpdatedAt: at}
	next.Tasks["other"] = &Task{ID: "other", State: StateRunning, Channel: "elsewhere", UpdatedAt: at}
	next.Tasks["child"] = &Task{ID: "child", Parent: "root", Origin: "delegate:root", State: parent, Channel: "chat", Result: &Result{}, Delivery: &Delivery{State: DeliveryDelivered}, UpdatedAt: at}
	next.Tasks["grandchild"] = &Task{ID: "grandchild", Parent: "child", Origin: "delegate:child", State: StateDone, Channel: "chat", Result: &Result{}, Delivery: &Delivery{State: DeliverySuppressed}, UpdatedAt: at.Add(-time.Hour)}
	if err := s.replaceData(next); err != nil {
		t.Fatal(err)
	}
	return s, book
}

func assertSuppressionReadViews(t *testing.T, s *Store, want bool) {
	t.Helper()
	head, ok := s.Header("root")
	if !ok || head.Summary.CanComplete != want {
		t.Errorf("production Header can_complete = %v (found %v), want %v", head.Summary.CanComplete, ok, want)
	}
	page, err := s.Query(Query{Scope: Scope{Kind: "children"}, Status: "live", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "root" || page.Items[0].Summary.CanComplete != want {
		t.Errorf("production Query root = %+v, %v; want can_complete %v", page, err, want)
	}
	found := false
	for _, h := range s.Workset(nil).Items {
		if h.ID == "root" {
			found = true
			if h.Summary.CanComplete != want || h.Attempts != nil {
				t.Errorf("production Workset root = %+v, want can_complete %v and no accounting rows", h, want)
			}
		}
	}
	if !found {
		t.Error("workset omitted the running root")
	}
	root, _ := s.Get("root")
	if eligible := CompletionBlocker(root, s.List("")) == nil; eligible != want {
		t.Fatalf("server predicate = %v, want %v", eligible, want)
	}
	if other, _ := s.Header("other"); !other.Summary.CanComplete {
		t.Error("unrelated root inherited the suppression blocker")
	}
	assertReadIndexMatchesStartup(t, s)
}

func TestSuppressedCompletionProductionHeaderAndQuery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*draft)
		want   bool
	}{
		{"done", func(*draft) {}, true},
		{"cancelled-parent", func(d *draft) { d.edit("child").State = StateCancelled }, true},
		{"cancelled-result", func(d *draft) { d.edit("grandchild").State = StateCancelled }, true},
		{"review-root", func(d *draft) { d.edit("root").State = StateReview }, true},
		{"queued", func(d *draft) { d.edit("grandchild").Delivery.State = DeliveryQueued }, false},
		{"uncertain", func(d *draft) { d.edit("grandchild").Delivery.State = DeliveryUncertain }, false},
		{"pending", func(d *draft) { d.edit("grandchild").Delivery.State = DeliveryPending }, false},
		{"running", func(d *draft) { d.edit("grandchild").State = StateRunning }, false},
		{"failed", func(d *draft) { d.edit("grandchild").State = StateFailed }, false},
		{"missing-result", func(d *draft) { d.edit("grandchild").Result = nil }, false},
		{"missing-delivery", func(d *draft) { d.edit("grandchild").Delivery = nil }, false},
		{"parent-running", func(d *draft) { d.edit("child").State = StateRunning }, false},
		{"parent-paused", func(d *draft) { d.edit("child").State = StatePaused }, false},
		{"parent-failed", func(d *draft) { d.edit("child").State = StateFailed }, false},
		{"not-delegated", func(d *draft) { d.edit("grandchild").Origin = "" }, false},
		{"direct-suppressed", func(d *draft) { d.edit("grandchild").Parent = "root" }, false},
		{"old-open-row", func(d *draft) {
			d.edit("grandchild").Attempts = []Attempt{{StartedAt: time.Now(), ExecutionEpoch: 99}}
		}, false},
		{"settled-failure", func(d *draft) {
			g := d.edit("grandchild")
			g.State, g.Settlement, g.Result, g.Delivery = StateFailed, SettlementHandled, nil, nil
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, book := suppressionReadStore(t, StateDone)
			d := s.draft()
			tc.change(d)
			if err := s.replaceLocked(d); err != nil {
				t.Fatal(err)
			}
			assertSuppressionReadViews(t, s, tc.want)
			reopened, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			assertSuppressionReadViews(t, reopened, tc.want)
		})
	}
}

func TestSuppressedCompletionParentDependencyUpdatesAfterCommit(t *testing.T) {
	for _, order := range []string{"parent-first", "parent-last", "same-write"} {
		t.Run(order, func(t *testing.T) {
			s, _ := suppressionReadStore(t, StateRunning)
			if order == "parent-first" {
				if err := s.SetDelivery("grandchild", DeliveryQueued); err != nil {
					t.Fatal(err)
				}
			}
			assertSuppressionReadViews(t, s, false)
			if order == "same-write" {
				d := s.draft()
				d.edit("child").State = StateDone
				d.edit("grandchild").Result.Answer = "committed together"
				if err := s.replaceLocked(d); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Advance("child", StateDone); err != nil {
					t.Fatal(err)
				}
				if order == "parent-first" {
					assertSuppressionReadViews(t, s, false)
					if err := s.SuppressDelivery("grandchild", "parent ended"); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertSuppressionReadViews(t, s, true)
		})
	}
}

func TestSuppressedCompletionParentDependencyRollsBackAndFencesCursor(t *testing.T) {
	s, book := suppressionReadStore(t, StateRunning)
	page, err := s.Query(Query{Scope: Scope{Kind: "children"}, Status: "live", Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("no roots cursor: %+v, %v", page, err)
	}
	q := Query{Scope: Scope{Kind: "children"}, Status: "live", Limit: 1, Cursor: page.NextCursor}
	before, err := s.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_suppression_parent BEFORE UPDATE ON bindings WHEN new.kind='task-store' BEGIN SELECT RAISE(ABORT,'refusal'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance("child", StateDone); err == nil {
		t.Fatal("fault did not refuse the parent write")
	}
	assertSuppressionReadViews(t, s, false)
	if after, err := s.Query(q); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused dependency update changed cursor/read index: %+v, %v", after, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_suppression_parent`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance("child", StateDone); err != nil {
		t.Fatal(err)
	}
	assertSuppressionReadViews(t, s, true)
	if _, err := s.Query(q); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("changed ancestor eligibility did not fence root page: %v", err)
	}
}

func TestSuppressedCompletionHeaderIncludesHiddenHistoricalDescendant(t *testing.T) {
	s, _ := suppressionReadStore(t, StateDone)
	d := s.draft()
	for i := range RecentClosedLimit + 1 {
		id := fmt.Sprintf("recent-%02d", i)
		d.add(&Task{ID: id, State: StateCancelled, UpdatedAt: time.Unix(200+int64(i), 0)})
	}
	if err := s.replaceLocked(d); err != nil {
		t.Fatal(err)
	}
	for _, h := range s.Workset(nil).Items {
		if h.ID == "grandchild" {
			t.Fatal("historical suppressed descendant must be outside bounded workset")
		}
	}
	assertSuppressionReadViews(t, s, true)
	if err := s.SetResult("grandchild", Result{Answer: "late detail"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDelivery("grandchild", DeliveryUncertain); err != nil {
		t.Fatal(err)
	}
	assertSuppressionReadViews(t, s, false)
}

func TestSuppressedCompletionDependencyMembershipStaysIncremental(t *testing.T) {
	s, _ := suppressionReadStore(t, StateDone)
	check := func(want bool) {
		t.Helper()
		assertSuppressionReadViews(t, s, want)
		fresh := &Store{data: s.data}
		fresh.rebuildReadIndexLocked()
		if !reflect.DeepEqual(s.readIndex.suppressed, fresh.readIndex.suppressed) {
			t.Fatalf("suppression reverse index differs from startup: got %v, want %v", s.readIndex.suppressed, fresh.readIndex.suppressed)
		}
	}
	check(true)
	for _, state := range []string{DeliveryQueued, DeliverySuppressed, DeliveryDelivered, DeliveryQueued, DeliverySuppressed} {
		if err := s.SetDelivery("grandchild", state); err != nil {
			t.Fatal(err)
		}
		check(state != DeliveryQueued)
	}
	// A whole stop writes the parent and dependent together; their blocker
	// delta must be counted once, irrespective of record-map iteration order.
	if _, err := s.SetAside("child", StateCancelled); err != nil {
		t.Fatal(err)
	}
	check(true)
	d := s.draft()
	d.edit("child").State = StateFailed
	d.edit("child").Settlement = SettlementHandled
	if err := s.replaceLocked(d); err != nil {
		t.Fatal(err)
	}
	check(false) // settled is not a done/cancelled direct parent
	d = s.draft()
	d.edit("child").State = StateDone
	if err := s.replaceLocked(d); err != nil {
		t.Fatal(err)
	}
	check(true)
	for _, parent := range []string{"root", "child"} {
		d = s.draft()
		d.edit("grandchild").Parent = parent
		if err := s.replaceLocked(d); err != nil {
			t.Fatal(err)
		}
		check(parent == "child")
	}
	d = s.draft()
	d.remove("grandchild")
	if err := s.replaceLocked(d); err != nil {
		t.Fatal(err)
	}
	check(true)
}

// Index-only measurement excludes SQLite setup/I/O. A parent state change
// costs its suppressed dependents and ancestor paths, not unrelated history.
func TestSuppressedCompletionIndexWriteCostIgnoresUnrelatedHistory(t *testing.T) {
	measure := func(history int) (float64, float64) {
		t.Helper()
		s := &Store{data: data{NextID: 1, Tasks: map[string]*Task{}, Meta: map[string]Meta{}}}
		s.data.Tasks["root"] = &Task{ID: "root", State: StateRunning}
		s.data.Tasks["child"] = &Task{ID: "child", Parent: "root", State: StateRunning, Result: &Result{}, Delivery: &Delivery{State: DeliveryDelivered}}
		s.data.Tasks["grandchild"] = &Task{ID: "grandchild", Parent: "child", Origin: "delegate:child", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliverySuppressed}}
		for i := range history {
			id := fmt.Sprintf("past-%05d", i)
			// Same parent, but ordinary delivered siblings are not dependencies.
			s.data.Tasks[id] = &Task{ID: id, Parent: "child", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliveryDelivered}}
		}
		s.rebuildReadIndexLocked()
		if len(s.readIndex.suppressed["child"]) != 1 {
			t.Fatal("reverse index included non-dependent history")
		}
		writes := testing.AllocsPerRun(4, func() {
			old := s.data.Tasks["child"]
			next := *old
			if old.State == StateRunning {
				next.State = StateDone
			} else {
				next.State = StateRunning
			}
			s.updateReadIndexLocked([]recordChange{{kind: taskKind, id: next.ID, value: headOf(&next)}}, func() { s.data.Tasks[next.ID] = &next })
			if h, _ := s.Header("root"); h.Summary.CanComplete != next.State.Terminal() {
				t.Fatal("parent update did not adjust its one dependent")
			}
		})
		reads := testing.AllocsPerRun(10, func() {
			if _, err := s.Query(Query{Scope: Scope{Kind: "children"}, Limit: 1}); err != nil {
				t.Fatal(err)
			}
		})
		return writes, reads
	}
	smallWrite, smallRead := measure(8)
	largeWrite, largeRead := measure(1000)
	t.Logf("8 -> 1000 ordinary siblings, one suppression dependency: write allocs %.0f -> %.0f; root-page allocs %.0f -> %.0f", smallWrite, largeWrite, smallRead, largeRead)
	if largeWrite > smallWrite+20 || largeRead > smallRead+2 {
		t.Fatal("suppression dependency maintenance/read grows with unrelated history")
	}
}

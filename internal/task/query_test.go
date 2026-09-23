package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func readFixture(t *testing.T, history int) (*Store, func(int)) {
	t.Helper()
	s, _ := taskRecordBook(t)
	seed := func(count int) {
		t.Helper()
		at := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
		next := s.clone()
		next.Tasks["root"] = &Task{ID: "root", State: StateDone, UpdatedAt: at, Channel: "thread", ProjectID: "p"}
		next.Tasks["live"] = &Task{ID: "live", Parent: "root", State: StatePaused, UpdatedAt: at, Channel: "thread", ProjectID: "p", Budget: Budget{Turns: count + 50}}
		next.Tasks["ref"] = &Task{ID: "ref", State: StateDone, UpdatedAt: at.Add(-time.Hour), Channel: "elsewhere"}
		next.Tasks["delivery"] = &Task{ID: "delivery", State: StateDone, Parent: "root", Origin: "delegate:root", Result: &Result{Answer: "pending"}, UpdatedAt: at}
		next.Tasks["failed"] = &Task{ID: "failed", State: StateFailed, UpdatedAt: at}
		next.Tasks["settled"] = &Task{ID: "settled", State: StateFailed, Settlement: SettlementHandled, UpdatedAt: at}
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("history-%05d", i)
			row := Attempt{StartedAt: at, EndedAt: at.Add(time.Second), ExecutionID: id, Model: "model", Tokens: Tokens{Total: 3}}
			next.Tasks[id] = &Task{ID: id, State: StateDone, UpdatedAt: at.Add(time.Second), Channel: "past", ProjectID: "p", Attempts: []Attempt{row}}
			next.Tasks["live"].Attempts = append(next.Tasks["live"].Attempts, row)
		}
		if err := s.replaceLocked(next); err != nil {
			t.Fatal(err)
		}
	}
	seed(history)
	return s, seed
}

func TestWorksetReadBoundedByLiveClosureAndRecentNotHistory(t *testing.T) {
	s, seed := readFixture(t, 25)
	inspect := func() (float64, int) {
		t.Helper()
		w := s.Workset([]string{"ref"})
		if w.Coverage.RecentLimit != 20 || w.Coverage.Live != 3 || len(w.Items) != 25 || !w.Coverage.HasMoreClosed {
			t.Fatalf("coverage/closure: %+v count=%d", w.Coverage, len(w.Items))
		}
		ids := map[string]bool{}
		for _, h := range w.Items {
			ids[h.ID] = true
			if h.Attempts != nil {
				t.Fatal("base header leaked accounting history")
			}
		}
		for _, id := range []string{"live", "failed", "delivery", "root", "ref"} {
			if !ids[id] {
				t.Fatalf("missing closure task %s", id)
			}
		}
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		allocs := testing.AllocsPerRun(20, func() { _ = s.Workset([]string{"ref"}) })
		return allocs, len(raw)
	}
	before, bytesBefore := inspect()
	seed(10000)
	after, bytesAfter := inspect()
	t.Logf("25 -> 10000 closed/own attempts: workset allocs %.0f -> %.0f, bytes %d -> %d", before, after, bytesBefore, bytesAfter)
	if after > before+10 || bytesAfter > bytesBefore+200 {
		t.Fatal("base read grows with historical tasks or attempts")
	}
	h, ok := s.Header("live")
	if !ok || h.Summary.Attempts != 10000 || h.Summary.Tokens.Total != 30000 || h.Summary.Seconds != 10000 || h.Budget.Turns != 10050 {
		t.Fatalf("own summary confused with subtree budget: %+v", h)
	}
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Workset(nil).Items, reopened.Workset(nil).Items) {
		t.Fatal("reopen changed indexed workset")
	}
}

func TestTaskQueryPagesAreScopedFencedAndComplete(t *testing.T) {
	s, _ := readFixture(t, 25)
	q := Query{Scope: Scope{Kind: "project", ID: "p"}, Limit: 1}
	seen := map[string]bool{}
	for {
		page, err := s.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := s.Query(q)
		if err != nil || !reflect.DeepEqual(page, repeated) {
			t.Fatal("repeated page changed", err)
		}
		if len(page.Items) != 1 || page.Total != 27 || seen[page.Items[0].ID] {
			t.Fatalf("missing/duplicate/equal-timestamp page: %+v", page)
		}
		seen[page.Items[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != 27 {
		t.Fatalf("lost history: %d", len(seen))
	}
	empty, err := s.Query(Query{Scope: Scope{Kind: "conversation", ID: "missing"}, Limit: 1})
	if err != nil || len(empty.Items) != 0 || empty.NextCursor != "" || empty.Total != 0 {
		t.Fatal("empty page", empty, err)
	}
	page, err := s.Query(Query{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Query{{Cursor: "1"}, {Cursor: "not-a-cursor"}, {Cursor: page.NextCursor, Scope: Scope{Kind: "project", ID: "p"}}, {Limit: MaxQueryLimit + 1}, {Limit: -1}, {Scope: Scope{Kind: "bogus"}}} {
		if _, err := s.Query(bad); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("invalid query accepted: %+v err=%v", bad, err)
		}
	}
	title := "updated"
	if _, err := s.SetMeta("live", MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(Query{Cursor: page.NextCursor, Limit: 1}); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("stale cursor did not explicitly demand refresh: %v", err)
	}
}

func TestTaskHeaderSummaryCompletionUsesFullTreeAndRollsBack(t *testing.T) {
	s, book := taskRecordBook(t)
	at := time.Now().UTC()
	next := s.clone()
	next.Tasks["root"] = &Task{ID: "root", State: StateRunning, UpdatedAt: at}
	next.Tasks["old-child"] = &Task{ID: "old-child", Parent: "root", State: StateDone, Result: &Result{Answer: "done"}, Delivery: &Delivery{State: DeliverySuppressed}, UpdatedAt: at.Add(-time.Hour)}
	if err := s.replaceLocked(next); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Header("root")
	if h.Summary.CanComplete || h.Summary.Children != 1 {
		t.Fatal("hidden child blocker lost", h)
	}
	old, _ := s.Query(Query{Limit: 1})
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_task_index BEFORE UPDATE ON bindings WHEN new.kind='task-store' BEGIN SELECT RAISE(ABORT,'refusal'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDelivery("old-child", DeliveryDelivered); err == nil {
		t.Fatal("fault did not fail mutation")
	}
	h, _ = s.Header("root")
	if h.Summary.CanComplete {
		t.Fatal("failed write installed new read index")
	}
	if _, err := s.Query(Query{Limit: 1, Cursor: old.NextCursor}); err != nil {
		t.Fatal("failed write invalidated cursor", err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_task_index`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDelivery("old-child", DeliveryDelivered); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Header("root")
	if !h.Summary.CanComplete {
		t.Fatal("completion summary not refreshed")
	}
	h, _ = s.Header("old-child")
	h.Result.Answer = "mutated caller"
	h.Delivery.State = DeliveryPending
	actual, _ := s.Header("old-child")
	if actual.Result.Answer == "mutated caller" || actual.Delivery.State != DeliveryDelivered {
		t.Fatal("header caller mutated owner")
	}
}

func TestTaskAttemptPagesBoundedWithEqualTimesAndInvalidCursors(t *testing.T) {
	s, _ := readFixture(t, 25)
	cursor := ""
	for want := 24; want >= 0; want-- {
		page, err := s.QueryAttempts("live", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 25 || len(page.Items) != 1 || page.Items[0].Index != want {
			t.Fatalf("accounting page: %+v", page)
		}
		again, err := s.QueryAttempts("live", cursor, 1)
		if err != nil || !reflect.DeepEqual(again, page) {
			t.Fatal("unstable page", err)
		}
		cursor = page.NextCursor
		if (want == 0) != (cursor == "") {
			t.Fatal("wrong terminal cursor")
		}
	}
	if _, err := s.QueryAttempts("live", "1", 1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("old numeric cursor accepted", err)
	}
	if _, err := s.QueryAttempts("missing", "", 1); err == nil {
		t.Fatal("missing task is not an empty history")
	}
	page, err := s.QueryAttempts("root", "", 1)
	if err != nil || page.Total != 0 || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatal("empty accounting history", page, err)
	}
}

func TestTaskPlanPresenceIncludesHiddenDescendantsAndTracksDeletion(t *testing.T) {
	s, _ := readFixture(t, 25)
	next := s.clone()
	next.Tasks["history-00000"].Parent = "root"
	next.Tasks["history-00000"].Channel = "delete-me"
	if err := s.replaceLocked(next); err != nil {
		t.Fatal(err)
	}
	s.SetPlanBindings([]string{"history-00000"})
	h, _ := s.Header("root")
	if !h.Summary.PlanInTree {
		t.Fatal("historical descendant plan disappeared from root summary")
	}
	w := s.Workset(nil)
	for _, h := range w.Items {
		if h.ID == "history-00000" {
			t.Fatal("fixture must hide historical descendant outside recent")
		}
	}
	if _, err := s.DeleteChannel("delete-me"); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Header("root")
	if h.Summary.PlanInTree {
		t.Fatal("deleted subtree left a phantom plan blocker")
	}
	// A separate root is never blocked by an orphan plan binding.
	h, _ = s.Header("live")
	if h.Summary.PlanInTree {
		t.Fatal("plan bound to unrelated root")
	}
}

func assertReadIndexMatchesStartup(t *testing.T, s *Store) {
	t.Helper()
	fresh := &Store{data: s.data, readIndex: readIndex{planTasks: s.readIndex.planTasks}}
	fresh.rebuildReadIndexLocked()
	for name, pair := range map[string][2]any{
		"ordered":     {s.readIndex.ordered, fresh.readIndex.ordered},
		"summaries":   {s.readIndex.summaries, fresh.readIndex.summaries},
		"counts":      {s.readIndex.counts, fresh.readIndex.counts},
		"models":      {s.readIndex.models, fresh.readIndex.models},
		"primary":     {s.readIndex.primary, fresh.readIndex.primary},
		"primaryRows": {s.readIndex.primaryRows, fresh.readIndex.primaryRows},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("incremental %s diverged from startup\n got=%+v\nwant=%+v", name, pair[0], pair[1])
		}
	}
}

func TestReadIndexIncrementalSummaryMembershipAndMutationRollback(t *testing.T) {
	s, _ := readFixture(t, 25)
	assertReadIndexMatchesStartup(t, s)
	s.SetPlanBindings([]string{"delivery", "history-00000"})
	assertReadIndexMatchesStartup(t, s)
	for round := 0; round < 30; round++ {
		next := s.clone()
		target := next.Tasks["live"]
		switch round % 6 {
		case 0:
			target.Attempts = append(target.Attempts, Attempt{StartedAt: target.UpdatedAt, ExecutionID: fmt.Sprint(round), Model: "latest"})
			target.State = StateRunning
		case 1:
			target.Attempts[len(target.Attempts)-1].EndedAt = target.UpdatedAt.Add(time.Second)
			target.Attempts[len(target.Attempts)-1].Tokens = Tokens{Input: 7, Total: 7}
		case 2:
			target.Attempts[1].Tokens = Tokens{Total: int64(round)}
			target.Attempts[1].Model = "old-updated"
			target.Attempts[len(target.Attempts)-1].Model = ""
		case 3:
			now := time.Now()
			next.Meta["live"] = Meta{Title: "organized", ArchivedAt: &now}
			target.Delivery = &Delivery{State: DeliveryUncertain}
		case 4:
			delete(next.Meta, "live")
			target.State = StateCancelled
			target.Delivery = &Delivery{State: DeliveryDelivered}
		case 5:
			target.State = StatePaused
			target.Attempts[len(target.Attempts)-1].Independent = true
		}
		target.UpdatedAt = target.UpdatedAt.Add(time.Second)
		if err := s.replaceLocked(next); err != nil {
			t.Fatal(err)
		}
		assertReadIndexMatchesStartup(t, s)
	}
	if _, err := s.DeleteChannel("past"); err != nil {
		t.Fatal(err)
	}
	assertReadIndexMatchesStartup(t, s)
	s.SetPlanBindings(nil)
	assertReadIndexMatchesStartup(t, s)
}

func TestOpenPrimaryAccountingUsesLastPrimaryRowAcrossAllStates(t *testing.T) {
	s, _ := readFixture(t, 25)
	next := s.clone()
	now := time.Now().UTC()
	for _, state := range []State{StatePaused, StateDone, StateCancelled, StateFailed, StateRunning} {
		id := string(state)
		next.Tasks[id] = &Task{ID: id, State: state, UpdatedAt: now, ExecutionEpoch: 99,
			Attempts: []Attempt{{StartedAt: now, EndedAt: now, ExecutionEpoch: 1}, {StartedAt: now, ExecutionEpoch: 2}, {StartedAt: now, Independent: true}},
			Result:   &Result{Answer: "header"}}
	}
	next.Tasks["closed-latest"] = &Task{ID: "closed-latest", State: StatePaused, Attempts: []Attempt{{StartedAt: now}, {StartedAt: now, EndedAt: now}}}
	next.Tasks["independent-only"] = &Task{ID: "independent-only", State: StateRunning, Attempts: []Attempt{{StartedAt: now, Independent: true}}}
	if err := s.replaceLocked(next); err != nil {
		t.Fatal(err)
	}
	got := s.OpenPrimaryAccounting()
	if len(got) != 5 {
		t.Fatalf("candidate semantics changed: %+v", got)
	}
	for _, candidate := range got {
		if candidate.Index != 1 || len(candidate.Task.Attempts) != 0 || candidate.Attempt.ExecutionEpoch != 2 {
			t.Fatal("row identity/header changed", candidate)
		}
	}
	got[0].Task.Result.Answer = "caller"
	if s.OpenPrimaryAccounting()[0].Task.Result.Answer == "caller" {
		t.Fatal("candidate aliases owner")
	}
	before := testing.AllocsPerRun(20, func() { _ = s.OpenPrimaryAccounting() })
	next = s.clone()
	for i := range 10000 {
		id := fmt.Sprintf("closed-candidate-%d", i)
		next.Tasks[id] = &Task{ID: id, State: StateDone}
	}
	// Same candidate count, but one candidate's accounting index is now 10k.
	tracked := next.Tasks[string(StatePaused)]
	tracked.Attempts = nil
	for i := range 10000 {
		tracked.Attempts = append(tracked.Attempts, Attempt{StartedAt: now, EndedAt: now, ExecutionID: fmt.Sprint(i)})
	}
	tracked.Attempts = append(tracked.Attempts, Attempt{StartedAt: now})
	if err := s.replaceLocked(next); err != nil {
		t.Fatal(err)
	}
	after := testing.AllocsPerRun(20, func() { _ = s.OpenPrimaryAccounting() })
	if after > before+2 {
		t.Fatalf("candidate polling grows with history: %.0f -> %.0f", before, after)
	}
	t.Logf("10k closed + own history, fixed5 candidates: allocations %.0f -> %.0f", before, after)
	assertReadIndexMatchesStartup(t, s)
}

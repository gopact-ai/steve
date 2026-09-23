package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestPlanHotReadsIgnoreClosedPlansAndRevisions(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	var bindings []string
	s.SetTaskProjection(func(ids []string) { bindings = append([]string(nil), ids...) })
	seed := func(count int) {
		next := s.clone()
		live := Plan{ID: "live", TaskID: "task-live", Steps: []Step{{ID: "go", State: StepAwaitingHuman}}}
		next.Plans["live"] = nil
		next.ByTask["task-live"] = "live"
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("p-%05d", i)
			taskID := "task-" + id
			p := Plan{ID: id, Rev: 1, TaskID: taskID, CreatedAt: time.Unix(1, 0), Steps: []Step{{ID: "old", State: StepDone}}}
			next.Plans[id] = []Plan{p}
			next.ByTask[taskID] = id
			past := live
			past.Rev = i + 1
			next.Plans["live"] = append(next.Plans["live"], past)
		}
		live.Rev = count + 1
		next.Plans["live"] = append(next.Plans["live"], live)
		ids := make([]string, 0, len(next.Plans))
		for id := range next.Plans {
			ids = append(ids, id)
		}
		if err := s.replaceLocked(next, ids); err != nil {
			t.Fatal(err)
		}
	}
	read := func() float64 {
		live := s.Live()
		selected := s.ForTasks([]string{"task-p-00000", "task-live", "absent", "task-live"})
		if len(live) != 1 || live[0].ID != "live" || len(selected) != 2 {
			t.Fatal("bad live/targeted projection", live, selected)
		}
		return testing.AllocsPerRun(20, func() { _ = s.Live(); _ = s.ForTasks([]string{"task-live", "task-p-00000"}) })
	}
	seed(25)
	before := read()
	seed(10000)
	after := read()
	if after > before+5 {
		t.Fatalf("hot plan read grows with history: %.0f -> %.0f", before, after)
	}
	t.Logf("25 -> 10000 closed plans/revisions: allocations %.0f -> %.0f", before, after)
	if len(bindings) != 10001 {
		t.Fatal("task projection silently lost closed plan bindings", len(bindings))
	}
	previous := append([]string(nil), bindings...)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_plan BEFORE UPDATE ON bindings BEGIN SELECT RAISE(ABORT,'plan refusal'); END`); err != nil {
		t.Fatal(err)
	}
	next := s.clone()
	delete(next.Plans, "live")
	if err := s.replaceLocked(next, []string{"live"}); err == nil {
		t.Fatal("expected store refusal")
	}
	if !reflect.DeepEqual(bindings, previous) || len(s.Live()) != 1 {
		t.Fatal("failed write published new plan projection")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_plan`); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Live(), reopened.Live()) {
		t.Fatal("reopen changed live plans")
	}
}

func TestPlanProgressUpdatesOnlyItsReadIndexAndNoTaskBindingRescan(t *testing.T) {
	s, err := Open(t.TempDir() + "/plans.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create(Plan{TaskID: "root", Steps: []Step{step("one", "do work")}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.SetTaskProjection(func(ids []string) {
		calls++
		if !reflect.DeepEqual(ids, []string{"root"}) {
			t.Fatal(ids)
		}
	})
	done := p.Steps[0]
	done.State = StepDone
	if _, err := s.Advance(p.ID, done); err != nil {
		t.Fatal(err)
	}
	if len(s.Live()) != 0 || calls != 1 {
		t.Fatalf("progress rebuilt task bindings/live selection: calls=%d live=%v", calls, s.Live())
	}
	if _, err := s.Revise(p.ID, []Step{step("two", "next")}, "owner", "continue"); err != nil {
		t.Fatal(err)
	}
	if len(s.Live()) != 1 || calls != 1 {
		t.Fatalf("revision rebuilt fixed bindings: calls=%d", calls)
	}
	// New membership is delivered synchronously after a successful create.
	if _, err := s.Create(Plan{TaskID: "root", Steps: []Step{step("other", "parallel")}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("another plan on same task changed the membership set")
	}
	got := s.ForTasks([]string{"root", "root"})
	if len(got) != 1 {
		t.Fatal("lost plan or duplicated target", got)
	}
	page, err := s.Query(Query{TaskID: "root", Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal("page", page, err)
	}
	same, err := s.Query(Query{TaskID: "root", Limit: 1})
	if err != nil || !reflect.DeepEqual(same, page) {
		t.Fatal("repeated page unstable", err)
	}
	last, err := s.Query(Query{TaskID: "root", Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].ID == page.Items[0].ID {
		t.Fatal("missing history page", last, err)
	}
	if _, err := s.Query(Query{Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("scope mismatch", err)
	}
	for _, cursor := range []string{"1", "not-a-cursor"} {
		if _, err := s.Query(Query{Cursor: cursor}); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("invalid cursor accepted", err)
		}
	}
	if _, err := s.Query(Query{Limit: MaxQueryLimit + 1}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("unbounded page accepted", err)
	}
	current, _ := s.Latest(p.ID)
	changed := current.Steps[0]
	changed.State = StepDone
	if _, err := s.Advance(p.ID, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(Query{TaskID: "root", Cursor: page.NextCursor}); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("mutating list cursor did not expire", err)
	}
	empty, err := s.Query(Query{TaskID: "missing"})
	if err != nil || len(empty.Items) != 0 || empty.NextCursor != "" || empty.Total != 0 {
		t.Fatal("empty page", empty, err)
	}
}

func TestPlanEqualTimestampPagesMatchIncrementalIndex(t *testing.T) {
	s, err := Open(t.TempDir() + "/plans.json")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(10, 0) }
	for i := range 12 {
		p, err := s.Create(Plan{TaskID: fmt.Sprint(i % 3), ProjectID: fmt.Sprint(i % 2), Steps: []Step{step("go", "work")}})
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			done := p.Steps[0]
			done.State = StepDone
			if _, err := s.Advance(p.ID, done); err != nil {
				t.Fatal(err)
			}
		}
		if i%3 == 0 {
			if _, err := s.Revise(p.ID, []Step{step("next", "revision")}, "owner", "changed intent"); err != nil {
				t.Fatal(err)
			}
		}
		rebuilt := &Store{data: s.data}
		rebuilt.rebuildReadIndexLocked()
		if !reflect.DeepEqual(s.readIndex.ordered, rebuilt.readIndex.ordered) || !reflect.DeepEqual(s.readIndex.live, rebuilt.readIndex.live) {
			t.Fatalf("incremental index differs from startup after mutation %d", i)
		}
	}
	for _, scope := range []Query{{}, {TaskID: "0"}, {ProjectID: "0"}, {TaskID: "0", ProjectID: "0"}} {
		all, err := s.Query(scope)
		if err != nil {
			t.Fatal(err)
		}
		q := scope
		q.Limit = 1
		var got []Plan
		for {
			page, err := s.Query(q)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page: %+v %v", page, err)
			}
			again, err := s.Query(q)
			if err != nil || !reflect.DeepEqual(page, again) {
				t.Fatal("repeated equal-time page changed", err)
			}
			got = append(got, page.Items...)
			if len(got) > all.Total {
				t.Fatal("page loop did not terminate")
			}
			if page.NextCursor == "" {
				break
			}
			q.Cursor = page.NextCursor
		}
		if !reflect.DeepEqual(got, all.Items) {
			t.Fatalf("scope %+v lost tied keys", scope)
		}
	}
}

func TestPlanReadRowsOwnNestedResultFields(t *testing.T) {
	s, err := Open(t.TempDir() + "/plans.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create(Plan{TaskID: "task", Steps: []Step{step("go", "work")}})
	if err != nil {
		t.Fatal(err)
	}
	progress := p.Steps[0]
	progress.Touches = []string{"original"}
	progress.Result = &StepResult{ExecutionToken: &task.ExecutionToken{TaskID: "task"}, Usage: &Usage{Input: 1}, Findings: []Finding{{Invalidates: []string{"original"}}}}
	if _, err := s.Advance(p.ID, progress); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(Query{})
	if err != nil {
		t.Fatal(err)
	}
	read := page.Items[0].Steps[0]
	read.Touches[0] = "changed"
	read.Result.Usage.Input = 9
	read.Result.ExecutionToken.TaskID = "changed"
	read.Result.Findings[0].Invalidates[0] = "changed"
	actual := s.Live()[0].Steps[0]
	if actual.Touches[0] != "original" || actual.Result.Usage.Input != 1 || actual.Result.ExecutionToken.TaskID != "task" || actual.Result.Findings[0].Invalidates[0] != "original" {
		t.Fatal("a detached read changed owner state")
	}
}

func TestPlanReadIndexDoesNotHideMalformedHistory(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`{"plans":{"p":[]}}`,
		`{"plans":{"p":[null]}}`,
		`{"plans":{"p":[{"id":"another","rev":1}]}}`,
		`{"plans":{"p":[{"id":"p","rev":2},{"id":"p","rev":1}]}}`,
		`{"plans":{"p":[{"id":"p","rev":1,"task_id":"task"}]},"by_task":{"other":"p"}}`,
		`{"plans":{},"by_task":{"task":"missing"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			if err := book.Document("plans").Save([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenLedger(book); err == nil {
				t.Fatal("corrupt owner history became an apparently complete query")
			}
		})
	}
}

func TestPlanMutationResultsAndInputsCannotChangeCommittedReadState(t *testing.T) {
	for _, method := range []string{"create", "revise", "advance"} {
		t.Run(method, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			s, err := OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Create(Plan{TaskID: "root", Steps: []Step{step("other", "other")}}); err != nil {
				t.Fatal(err)
			}
			nested := step("go", "work")
			nested.Touches = []string{"original"}
			nested.Result = &StepResult{
				ExecutionToken: &task.ExecutionToken{TaskID: "root"},
				Usage:          &Usage{Input: 1},
				Findings:       []Finding{{Invalidates: []string{"original"}}},
			}
			input := Plan{TaskID: "root", Execution: &task.ExecutionToken{TaskID: "root"}, Steps: []Step{nested}}
			result, err := s.Create(input)
			if err != nil {
				t.Fatal(err)
			}
			switch method {
			case "revise":
				result, err = s.Revise(result.ID, input.Steps, "owner", "new intent")
			case "advance":
				result, err = s.Advance(result.ID, nested)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.Latest(result.ID)
			live := s.Live()
			first, err := s.Query(Query{TaskID: "root", Limit: 1})
			if err != nil || first.NextCursor == "" {
				t.Fatal("missing cursor fixture", err)
			}
			tail, err := s.Query(Query{TaskID: "root", Limit: 1, Cursor: first.NextCursor})
			if err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				actual, _ := s.Latest(result.ID)
				got, err := s.Query(Query{TaskID: "root", Limit: 1})
				last, lastErr := s.Query(Query{TaskID: "root", Limit: 1, Cursor: first.NextCursor})
				if !reflect.DeepEqual(before, actual) || !reflect.DeepEqual(live, s.Live()) ||
					err != nil || lastErr != nil || !reflect.DeepEqual(first, got) || !reflect.DeepEqual(tail, last) {
					t.Fatal("caller mutation bypassed durable state, read index or cursor")
				}
				reopened, err := OpenLedger(book)
				if err != nil {
					t.Fatal(err)
				}
				persisted, _ := reopened.Latest(result.ID)
				// JSON does not preserve monotonic time, but the public
				// serialized state must still equal the committed record.
				want, _ := json.Marshal(before)
				have, _ := json.Marshal(persisted)
				if !bytes.Equal(want, have) {
					t.Fatal("reopen differs from committed projection")
				}
			}
			result.Execution.Epoch = 99
			result.Steps[0].State = StepDone
			result.Steps[0].Touches[0] = "returned"
			result.Steps[0].Result.ExecutionToken.TaskID = "returned"
			result.Steps[0].Result.Usage.Input = 99
			result.Steps[0].Result.Findings[0].Invalidates[0] = "returned"
			check()
			input.Execution.TaskID = "input"
			input.Steps[0].State = StepFailed
			nested.Touches[0] = "input"
			nested.Result.Usage.Input = 100
			nested.Result.ExecutionToken.TaskID = "input"
			nested.Result.Findings[0].Invalidates[0] = "input"
			check()
		})
	}
}

package plan

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestScopedPlanPagesSurviveUnrelatedPlanProgress(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	var selected Plan
	for range 3 {
		selected, err = s.Create(Plan{TaskID: "selected", ProjectID: "project-a", Steps: []Step{step("go", "work")}})
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := s.Create(Plan{TaskID: "other", ProjectID: "project-b", Steps: []Step{step("go", "work")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []Query{{TaskID: "selected"}, {ProjectID: "project-a"}, {TaskID: "selected", ProjectID: "project-a"}} {
		query.Limit = 1
		first, err := s.Query(query)
		if err != nil || first.NextCursor == "" {
			t.Fatalf("missing plan page: %+v %v", first, err)
		}
		query.Cursor = first.NextCursor
		want, err := s.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []StepState{StepRunning, StepDone} {
			progress := other.Steps[0]
			progress.State = state
			if _, err := s.Advance(other.ID, progress); err != nil {
				t.Fatal(err)
			}
			got, err := s.Query(query)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("unrelated plan changed stable scope %+v: %+v %v", query, got, err)
			}
		}
		if _, err := s.Revise(selected.ID, []Step{step("next", "new work")}, "owner", "new intent"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Query(query); !errors.Is(err, ErrStaleCursor) {
			t.Fatalf("selected revision did not invalidate plan page: %v", err)
		}
	}
}

func TestPlanCursorVersionsRollbackRebuildAndDropEmptyScopes(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 3 {
		p, err := s.Create(Plan{TaskID: "selected", ProjectID: "project", Steps: []Step{step("go", "work")}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	q := Query{TaskID: "selected", Limit: 1}
	first, err := s.Query(q)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("missing plan page: %+v %v", first, err)
	}
	q.Cursor = first.NextCursor
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_plan_cursor BEFORE UPDATE ON bindings BEGIN SELECT RAISE(ABORT,'refusal'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revise(ids[0], []Step{step("new", "next")}, "owner", "new intent"); err == nil {
		t.Fatal("write did not hit injected failure")
	}
	if _, err := s.Query(q); err != nil {
		t.Fatalf("failed mutation invalidated cursor: %v", err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_plan_cursor`); err != nil {
		t.Fatal(err)
	}
	original := s.clone()
	empty := s.clone()
	for _, id := range ids {
		delete(empty.Plans, id)
	}
	delete(empty.ByTask, "selected")
	if err := s.replaceLocked(empty, ids); err != nil {
		t.Fatal(err)
	}
	if len(s.readIndex.versions) != 0 {
		t.Fatalf("deleted scope retained %d cursor tombstones", len(s.readIndex.versions))
	}
	if err := s.replaceLocked(original, ids); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(q); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("recreated scope accepted old cursor: %v", err)
	}
	fresh, err := s.Query(Query{TaskID: "selected", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = fresh.NextCursor
	reopened, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Query(q); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("reopened owner accepted an old instance cursor: %v", err)
	}
	s.rebuildReadIndexLocked()
	if _, err := s.Query(q); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("rebuilt index accepted an old cursor: %v", err)
	}
}

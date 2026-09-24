package task

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestScopedTaskAndAccountingPagesSurviveUnrelatedWrites(t *testing.T) {
	s, _ := readFixture(t, 25)
	for _, scope := range []Scope{{Kind: "project", ID: "p"}, {Kind: "conversation", ID: "thread"}, {Kind: "children", ID: "root"}} {
		t.Run(scope.Kind, func(t *testing.T) {
			first, err := s.Query(Query{Scope: scope, Limit: 1})
			if err != nil || first.NextCursor == "" {
				t.Fatalf("missing first page: %+v %v", first, err)
			}
			q := Query{Scope: scope, Limit: 1, Cursor: first.NextCursor}
			want, err := s.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for _, title := range []string{"one", "two", "three"} {
				if _, err := s.SetMeta("ref", MetaPatch{Title: &title}); err != nil {
					t.Fatal(err)
				}
				got, err := s.Query(q)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("unrelated task invalidated stable %s page: %+v %v", scope.Kind, got, err)
				}
			}
			title := fmt.Sprintf("selected changed: %s", scope.Kind)
			if _, err := s.SetMeta("live", MetaPatch{Title: &title}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Query(q); !errors.Is(err, ErrStaleCursor) {
				t.Fatalf("selected task mutation did not invalidate its page: %v", err)
			}
		})
	}
	page, err := s.QueryAttempts("live", "", 1)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("missing accounting page: %+v %v", page, err)
	}
	want, err := s.QueryAttempts("live", page.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ref", "live"} {
		title := "only metadata changed"
		if _, err := s.SetMeta(id, MetaPatch{Title: &title}); err != nil {
			t.Fatal(err)
		}
		got, err := s.QueryAttempts("live", page.NextCursor, 1)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("metadata changed unchanged accounting for %s: %+v %v", id, got, err)
		}
	}
	next := s.clone()
	next.Tasks["live"].Attempts[0].Tokens.Total++
	if err := s.replaceData(next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueryAttempts("live", page.NextCursor, 1); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("selected accounting mutation did not invalidate its page: %v", err)
	}
}

func TestScopedTaskCursorTracksAncestorsFiltersAndFailedWrites(t *testing.T) {
	s, _ := readFixture(t, 25)
	closed := Query{Status: "closed", Scope: Scope{Kind: "project", ID: "p"}, Limit: 1}
	first, err := s.Query(closed)
	if err != nil {
		t.Fatal(err)
	}
	closed.Cursor = first.NextCursor
	title := "unrelated live payload"
	if _, err := s.SetMeta("live", MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(closed); err != nil {
		t.Fatalf("a live-only change invalidated a closed-only page: %v", err)
	}
	roots, err := s.Query(Query{Scope: Scope{Kind: "children"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.SetPlanBindings([]string{"live"})
	if _, err := s.Query(Query{Scope: Scope{Kind: "children"}, Limit: 1, Cursor: roots.NextCursor}); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("descendant plan changed root summary without invalidating root page: %v", err)
	}
	page, err := s.QueryAttempts("live", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.book.DB().Exec(`CREATE TRIGGER reject_cursor BEFORE UPDATE ON bindings WHEN new.kind='task-store' BEGIN SELECT RAISE(ABORT,'refusal'); END`); err != nil {
		t.Fatal(err)
	}
	next := s.clone()
	next.Tasks["live"].Attempts[0].Tokens.Total++
	if err := s.replaceData(next); err == nil {
		t.Fatal("accounting mutation bypassed the injected failure")
	}
	if _, err := s.QueryAttempts("live", page.NextCursor, 1); err != nil {
		t.Fatalf("failed write advanced accounting cursor: %v", err)
	}
	if _, err := s.book.DB().Exec(`DROP TRIGGER reject_cursor`); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.QueryAttempts("live", page.NextCursor, 1); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("another owner instance accepted an old cursor: %v", err)
	}
	// Rebuilding the projection also changes its instance identity.
	s.rebuildReadIndexLocked()
	if _, err := s.QueryAttempts("live", page.NextCursor, 1); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("rebuilt owner accepted an old cursor: %v", err)
	}
}

func TestTaskCursorVersionsForgetEmptyScopesWithoutReusingIdentity(t *testing.T) {
	s, _ := readFixture(t, 3)
	query := Query{Scope: Scope{Kind: "conversation", ID: "past"}, Limit: 1}
	first, err := s.Query(query)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("missing old scope page: %+v %v", first, err)
	}
	query.Cursor = first.NextCursor
	original := s.clone()
	if _, err := s.DeleteChannel("past"); err != nil {
		t.Fatal(err)
	}
	for key := range s.readIndex.versions {
		if len(s.readIndex.ordered[key]) != 0 {
			continue
		}
		if key.Kind != "accounting" || s.data.Tasks[key.ID] == nil || len(s.data.Tasks[key.ID].Attempts) == 0 {
			t.Fatalf("empty scope retained a cursor tombstone: %+v", key)
		}
	}
	if err := s.replaceData(original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(query); !errors.Is(err, ErrStaleCursor) {
		t.Fatalf("recreated identical scope accepted a pre-deletion cursor: %v", err)
	}
}

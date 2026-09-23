package attempt

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestForTasksByUpdateListsOnlyThoseTasksMostRecentlyUpdatedFirst(t *testing.T) {
	s := identityStore(t)
	for _, row := range []struct{ id, task, state, updated string }{
		{"a-old", "a", "bound", "2026-09-01T00:00:01Z"},
		{"b-new", "b", "failed", "2026-09-01T00:00:05Z"},
		{"a-running", "a", "running", "2026-09-01T00:00:03Z"},
		{"a-tie", "a", "bound", "2026-09-01T00:00:05Z"},
		{"c-newest", "c", "bound", "2026-09-01T00:00:09Z"},
	} {
		raw := fmt.Sprintf(`{"id":%q,"task_id":%q}`, row.id, row.task)
		if _, err := s.l.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,?,?)`, row.id, row.state, raw, row.updated, row.updated); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ForTasksByUpdate(t.Context(), []string{"a", "b", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"b-new", "a-tie", "a-running", "a-old"}) {
		t.Fatalf("task attempts = %v", ids)
	}
	if none, err := s.ForTasksByUpdate(t.Context(), nil); err != nil || len(none) != 0 {
		t.Fatalf("no tasks = %+v %v", none, err)
	}
	insertIdentityRecord(t, s, "bad", "bound", `{`)
	if _, err := s.ForTasksByUpdate(t.Context(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("malformed history hidden: %v", err)
	}
}

func TestForTasksByUpdateSeeksTheTaskIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	fixture := seedSettledHistory(t, book, 2000)
	got, err := New(book).ForTasksByUpdate(t.Context(), []string{fixture.delegateTask})
	if err != nil || len(got) != 1 || got[0].TaskID != fixture.delegateTask {
		t.Fatalf("task attempts = %+v %v", got, err)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if plan := queryPlan(t, diagnostic, tasksByUpdateSQL, `["`+fixture.delegateTask+`"]`); !strings.Contains(plan, "operations_attempt_task") || strings.Contains(plan, "SCAN operations\n") {
		t.Fatalf("unbounded query plan:\n%s", plan)
	}
}

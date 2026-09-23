package delegate

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// A child recovers from the first eligible record in this order: every live
// attempt, most recently updated first, then the settled attempts of children
// that still owe their parent, most recently updated first. Settled history
// of anything else is not read.
func TestRecoverableRecordsKeepLiveFirstThenSettledByUpdate(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	root, err := tasks.Create(task.Task{Goal: "root", Channel: "chat", Member: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := tasks.Spawn(root.ID, task.Task{Goal: "child", Member: "builder", Origin: "delegate:" + root.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := tasks.Spawn(root.ID, task.Task{Goal: "not delegated", Member: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, task, state, extra string
		second                 int
	}{
		{"c-old", child.ID, "bound", "", 1},
		{"live-old", child.ID, "running", "", 2},
		{"c-tie-a", child.ID, "bound", "", 4},
		{"c-tie-b", child.ID, "expired", "", 4},
		{"c-new", child.ID, "failed", "", 6},
		{"live-new", other.ID, "prepared", "", 7},
		{"other-settled", other.ID, "bound", "", 8},
		{"unsettled", child.ID, "failed", `,"unsettled":true`, 9},
	} {
		raw := fmt.Sprintf(`{"id":%q,"task_id":%q,"kind":"delegate"%s}`, row.id, row.task, row.extra)
		at := fmt.Sprintf("2026-09-01T00:00:%02dZ", row.second)
		if _, err := book.DB().Exec(`INSERT INTO operations VALUES(?,'attempt',?,1,1,?,?,?)`, row.id, row.state, raw, at, at); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{tasks: tasks, attempts: attempt.New(book)}
	records, err := s.recoverableRecords(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range records {
		got = append(got, r.ID)
	}
	want := []string{"unsettled", "live-new", "live-old", "c-new", "c-tie-b", "c-tie-a", "c-old"}
	if !slices.Equal(got, want) {
		t.Fatalf("recovery order = %v, want %v", got, want)
	}
	if _, err := book.DB().Exec(`INSERT INTO operations VALUES('broken','attempt','bound',1,1,'{','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.recoverableRecords(t.Context()); err == nil {
		t.Fatal("malformed history did not fail the recovery pass")
	}
}

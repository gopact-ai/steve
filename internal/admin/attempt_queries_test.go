package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestAttemptHistoryPagesOpenTheExactNativeFileSnapshot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	work := t.TempDir()
	projects := project.Open(book)
	p := project.Project{ID: "p", Home: project.Home{Path: work}, Level: project.LevelPublic}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	snapshot := func(text, parent string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "evidence.txt"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		m, _, err := artifacts.SnapshotCanonical(t.Context(), p, parent, "test", text)
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	base := snapshot("base", "")
	old := snapshot("older result", base)
	newest := snapshot("old task newly completed", old)
	for _, id := range []string{"old-task", "new-task"} {
		if err := book.PutBinding(t.Context(), "task", id, map[string]any{"id": id, "transport": "console", "channel": "opaque"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []attempt.Record{
		{Spec: attempt.Spec{ID: "old-execution", TaskID: "new-task", Project: p.ID, Base: base}, StartedAt: time.Unix(8, 0), EndedAt: time.Unix(9, 0), Result: &attempt.Result{Artifact: old}},
		{Spec: attempt.Spec{ID: "new-execution", TaskID: "old-task", Project: p.ID, Base: old}, StartedAt: time.Unix(1, 0), EndedAt: time.Unix(10, 0), Result: &attempt.Result{Artifact: newest}},
	} {
		if _, err := book.BeginGuarded(t.Context(), r.ID, "attempt", string(attempt.Bound), "test", r, func(tx *ledger.Tx) error {
			raw, _ := json.Marshal(r)
			return attempt.ImportHistoryTx(tx, []ledger.Operation{{ID: r.ID, Kind: "attempt", State: string(attempt.Bound), Data: raw}}, false)
		}); err != nil {
			t.Fatal(err)
		}
	}
	a := &Service{Attempts: attempt.New(book), Artifacts: artifacts}
	var native consoleapi.AttemptQueries = a
	q := consoleapi.AttemptHistoryQuery{Conversation: "opaque", Limit: 1}
	for _, want := range []struct{ id, task, text string }{
		{"new-execution", "old-task", "old task newly completed"},
		{"old-execution", "new-task", "older result"},
	} {
		page, err := native.QueryAttempts(t.Context(), q)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != want.id || page.Items[0].TaskID != want.task ||
			!page.Items[0].FilesKnown || page.Items[0].Files != 1 {
			t.Fatalf("native file choice: %+v %v", page, err)
		}
		file, err := a.AttemptFile(t.Context(), page.Items[0].ID, "evidence.txt")
		if err != nil || file.Text != want.text || file.Attempt != want.id {
			t.Fatalf("file selection mixed executions: %+v %v", file, err)
		}
		q.Cursor = page.NextCursor
	}
	if q.Cursor != "" {
		t.Fatal("native page never reached the end")
	}
}

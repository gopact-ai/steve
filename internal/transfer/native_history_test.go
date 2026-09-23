package transfer

import (
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestNativeHistoryRemainsPageableAfterOfflineProjectTransfer(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	native := attempt.New(book)
	for _, id := range []string{"native-a", "native-b", "native-c"} {
		if _, err := native.Open(t.Context(), attempt.Spec{ID: id, TaskID: "1", Project: "p", Scope: attempt.ScopeNone}); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Fail(t.Context(), id, "fixture", "closed before transfer"); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "project.steve")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Evidence: "stopped", Output: bundle}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	destination, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{ProjectID: "existing", Goal: "force task-ID remap"}); err != nil {
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Home: filepath.Join(t.TempDir(), "home"), Input: bundle}); err != nil {
		t.Fatal(err)
	}
	destination, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	tasks, err = task.OpenLedger(destination)
	if err != nil {
		t.Fatal(err)
	}
	moved := []task.Task{}
	for _, item := range tasks.List("") {
		if item.ProjectID == "p" {
			moved = append(moved, item)
		}
	}
	if len(moved) != 1 || moved[0].ID == "1" {
		t.Fatalf("fixture did not remap task: %+v", moved)
	}
	query := attempt.HistoryQuery{TaskID: moved[0].ID, Limit: 1}
	seen := map[string]bool{}
	for {
		page, err := attempt.New(destination).QueryHistory(t.Context(), query)
		if err != nil || len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatalf("imported native cursor lost records: %+v %v", page, err)
		}
		seen[page.Items[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("imported only %d native records", len(seen))
	}
}

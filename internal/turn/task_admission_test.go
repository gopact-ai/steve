package turn

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskPersistenceFailureRefusesExecutionAndCanRetry(t *testing.T) {
	for _, stage := range []string{"create", "begin", "close-previous-project"} {
		t.Run(stage, func(t *testing.T) {
			runner := &fakeRunner{reply: "done"}
			c, _ := taskCoordinator(t, runner)
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = book.Close() })
			tasks, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			c.SetTasks(tasks, "hub")
			if stage != "create" {
				projectID := "codex"
				if stage == "close-previous-project" {
					projectID = "previous-project"
				}
				if _, err := tasks.Create(task.Task{Goal: "existing goal", Channel: "chat", Member: "codex", ProjectID: projectID}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := json.Marshal(tasks.List(""))
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_task_admission BEFORE INSERT ON bindings
					WHEN NEW.kind = 'document' AND NEW.id = 'tasks'
					BEGIN SELECT RAISE(ABORT, 'task admission unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err = handle(c, t.Context(), "perform work")
			if err == nil || !strings.Contains(err.Error(), "task admission unavailable") {
				t.Errorf("task storage failure was hidden: %v", err)
			}
			if got := runner.seen(); len(got) != 0 {
				t.Errorf("unadmitted work reached the agent: %v", got)
			}
			manager := c.runtime.(*fakeManager)
			if len(manager.opened) != 0 {
				t.Errorf("unadmitted work opened native sessions: %v", manager.opened)
			}
			after, err := json.Marshal(tasks.List(""))
			if err != nil || string(before) != string(after) {
				t.Errorf("rejected admission changed task state: before=%s after=%s err=%v", before, after, err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`DROP TRIGGER reject_task_admission`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := handle(c, t.Context(), "perform work"); err != nil {
				t.Fatalf("retry after storage recovery: %v", err)
			}
			if got := runner.seen(); len(got) != 1 {
				t.Fatalf("retry should execute exactly once, got %d prompts", len(got))
			}
		})
	}
}

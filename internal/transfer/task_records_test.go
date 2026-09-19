package transfer

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestReleaseOwnershipAndTaskRecordFreezeRollbackTogether(t *testing.T) {
	dir, _ := transferFixture(t)
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin("1", "worker", "node", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Finish("1", task.OutcomeOK, task.Tokens{Input: 7, Total: 7}, 1); err != nil {
		t.Fatal(err)
	}
	var b Bundle
	if err := exportDocuments(t.Context(), book, "p", &b); err != nil {
		t.Fatal(err)
	}
	before := b.Tasks.Tasks["1"]
	if before == nil || len(before.Attempts) != 1 {
		t.Fatal("fixture did not export actual task accounting")
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_owner_release BEFORE UPDATE ON bindings WHEN NEW.kind='project-owner' BEGIN SELECT RAISE(ABORT,'owner release rejected'); END`); err != nil {
		t.Fatal(err)
	}
	options := Options{TargetHub: "target", TransferID: "task-transfer", Evidence: "stopped"}
	if err := releaseSource(t.Context(), projects, options, "p", nil, b.Tasks); err == nil || !strings.Contains(err.Error(), "owner release rejected") {
		t.Fatalf("ownership failure: %v", err)
	}
	loaded, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	got, found := loaded.Get("1")
	if !found || !reflect.DeepEqual(*before, got) {
		t.Fatal("failed ownership release committed task freeze or lost accounting")
	}
	owner, found, err := projects.Ownership(t.Context(), "p")
	if err != nil || !found || owner.State != "active" {
		t.Fatalf("failed release changed ownership: %+v %v", owner, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_owner_release`); err != nil {
		t.Fatal(err)
	}
	if err := releaseSource(t.Context(), projects, options, "p", nil, b.Tasks); err != nil {
		t.Fatal(err)
	}
	loaded, err = task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	got, found = loaded.Get("1")
	if !found || got.State != task.StateCancelled || got.ExecutionEpoch != before.ExecutionEpoch+1 || got.Budget != before.Budget || !reflect.DeepEqual(got.Attempts, before.Attempts) {
		t.Fatalf("freeze lost accounting or retained epoch: %+v", got)
	}
	if _, _, err := projects.Get(t.Context(), "p"); !errors.Is(err, project.ErrNotOwner) {
		t.Fatalf("source ownership survived freeze: %v", err)
	}
}

func TestImportFinalTaskRecordFailureRollsBackAllDomainFactsAndRetries(t *testing.T) {
	source, _ := transferFixture(t)
	path := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	book, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_task_import BEFORE INSERT ON bindings WHEN NEW.kind='task-store' BEGIN SELECT RAISE(ABORT,'task import rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	options := ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: filepath.Join(t.TempDir(), "imported")}
	if _, err := Import(t.Context(), options); err == nil || !strings.Contains(err.Error(), "task import rejected") {
		t.Fatalf("import failure: %v", err)
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"operations", "events", "bindings", "names"} {
		var count int
		if err := book.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("task failure partially committed %s: %d %v", table, count, err)
		}
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_task_import`); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := Import(t.Context(), options); err != nil {
			t.Fatal("retry", err)
		}
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	loaded, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	got, found := loaded.Get(namespace(b.Owner.HubID, "1"))
	if !found || got.Goal != "project history" || len(loaded.List("")) != 1 {
		t.Fatalf("retry omitted or duplicated task: %+v", got)
	}
	if _, found, err := book.Document("tasks").Load(); err != nil || found {
		t.Fatal("import restored the obsolete shared task document", err)
	}
}

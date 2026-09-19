package transfer

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func seedTransferConsole(t *testing.T, book *ledger.Ledger) {
	t.Helper()
	saved := console.DurableState{
		Replies: map[string][]consoleapi.Reply{"console:move": {{ID: "reply", Conversation: "console:move", ProjectID: "p", Text: "retained evidence"}}},
		Exchanges: map[string][]console.DurableExchange{"console:move": {{
			Exchange:    consoleapi.Exchange{ID: "queued", Conversation: "console:move", ExpectedProject: "p", ExpectedTask: "1", State: consoleapi.ExchangeQueued, Key: "client:stable"},
			PayloadHash: "hash",
		}}},
		Questions: map[string]consoleapi.PendingQuestion{"question": {ID: "question", Conversation: "console:move", Project: "p", TaskID: "1", State: "pending"}},
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, saved) }); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleReleaseRefusesChangedExportAndRollsBackAllOwners(t *testing.T) {
	for _, failure := range []string{"console-control", "project-owner", "changed-export"} {
		t.Run(failure, func(t *testing.T) {
			dir, _ := transferFixture(t)
			book, err := ledger.Open(dir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			seedTransferConsole(t, book)
			var b Bundle
			if err := exportDocuments(t.Context(), book, "p", &b); err != nil {
				t.Fatal(err)
			}
			before, err := console.LoadState(book)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "changed-export" {
				before.Exchanges["console:move"][0].Input = "changed since export"
				if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, before) }); err != nil {
					t.Fatal(err)
				}
			} else {
				kind := "console-store"
				if failure == "project-owner" {
					kind = "project-owner"
				}
				if _, err := book.DB().Exec(`CREATE TRIGGER reject_release BEFORE UPDATE ON bindings WHEN new.kind='` + kind + `' BEGIN SELECT RAISE(ABORT, 'release refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			projects := project.Open(book)
			projects.SetHubID("source")
			options := Options{TargetHub: "target", TransferID: "console-transfer", Evidence: "stopped"}
			err = releaseSource(t.Context(), projects, options, "p", nil, b.Tasks, b.Console)
			if err == nil || (failure == "changed-export" && !errors.Is(err, ledger.ErrConflict)) {
				t.Fatalf("release did not refuse %s: %v", failure, err)
			}
			after, err := console.LoadState(book)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed release committed console freeze", err)
			}
			loaded, err := task.OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			got, ok := loaded.Get("1")
			if !ok || !reflect.DeepEqual(got, *b.Tasks.Tasks["1"]) {
				t.Fatal("failed console release committed task epoch or state")
			}
			owner, ok, err := projects.Ownership(t.Context(), "p")
			if err != nil || !ok || owner.State != "active" {
				t.Fatal("failed console release changed project owner", err)
			}
		})
	}
}

func TestConsoleImportLateRefusalRollsBackTaskFactsAndRetries(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedTransferConsole(t, book)
	book.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: path, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_console_import BEFORE INSERT ON bindings WHEN new.kind='console-store' BEGIN SELECT RAISE(ABORT, 'console import rejected'); END`); err != nil {
		t.Fatal(err)
	}
	book.Close()
	options := ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: path, Home: filepath.Join(t.TempDir(), "imported")}
	if _, err := Import(t.Context(), options); err == nil || !strings.Contains(err.Error(), "console import rejected") {
		t.Fatalf("late console refusal: %v", err)
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"operations", "events", "bindings", "names"} {
		var count int
		if err := book.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("console failure partially committed %s: %d %v", table, count, err)
		}
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_console_import`); err != nil {
		t.Fatal(err)
	}
	book.Close()
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
	saved, err := console.LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	conv := consoleNamespace("source", "p", "console:move")
	if len(saved.Replies[conv]) != 1 || len(saved.Exchanges[conv]) != 1 || saved.Exchanges[conv][0].PayloadHash != "hash" ||
		saved.Exchanges[conv][0].ExpectedTask != namespace("source", "1") || saved.Questions["question"].State != "interrupted" {
		t.Fatalf("retry lost/duplicated owner records: %+v", saved)
	}
	if _, exists, err := book.Document("console").Load(); err != nil || exists {
		t.Fatal("record transfer recreated document authority", err)
	}
}

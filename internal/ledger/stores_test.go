package ledger_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// State and plan retain their one-time document import contract. Task records
// deliberately do not share that contract; see the separate test below.
func TestStoresImportLegacyFilesOnceAndPersistInLedger(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	plansPath := filepath.Join(dir, "plans.json")

	oldState, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldState.SetActiveAgent("oc_1", "claude"); err != nil {
		t.Fatal(err)
	}
	oldPlans, err := plan.Open(plansPath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := oldPlans.Create(plan.Plan{TaskID: "legacy-task", Steps: []plan.Step{{ID: "s1", Goal: "one", Agent: "claude", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "smoke"}}}})
	if err != nil {
		t.Fatal(err)
	}

	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	newState, err := state.OpenLedger(book, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := newState.Conversation("oc_1").ActiveAgent; got != "claude" {
		t.Fatalf("active agent after import = %q", got)
	}
	newPlans, err := plan.OpenLedger(book, plansPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := newPlans.ForTask(created.TaskID); !ok || got.ID != created.ID || len(got.Steps) != 1 {
		t.Fatalf("plan after import = %+v ok=%v", got, ok)
	}
	for _, p := range []string{statePath, plansPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists as authority", p)
		}
		if _, err := os.Stat(p + ".migrated"); err != nil {
			t.Fatalf("%s was not retired: %v", p, err)
		}
	}

	// Writes land in the ledger and survive a reopen; the retired file is
	// not consulted even if it reappears.
	if err := newState.SetActiveAgent("oc_1", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := newPlans.Revise(created.ID, created.Steps, "codex", "current ledger revision"); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{statePath, plansPath} {
		if err := os.WriteFile(p, []byte("stray file is not authority"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	book2, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book2.Close()
	againState, err := state.OpenLedger(book2, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := againState.Conversation("oc_1").ActiveAgent; got != "codex" {
		t.Fatalf("current state after reopen = %q", got)
	}
	againPlans, err := plan.OpenLedger(book2, plansPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := againPlans.ForTask(created.TaskID); !ok || got.ID != created.ID || got.Rev != 2 || len(got.Steps) != 1 || got.Because != "current ledger revision" {
		t.Fatalf("current plan after reopen = %+v ok=%v", got, ok)
	}
	for _, p := range []string{statePath, plansPath} {
		if raw, err := os.ReadFile(p); err != nil || string(raw) != "stray file is not authority" {
			t.Fatalf("stray file was consumed or overwritten: %s %v", p, err)
		}
	}
}

func TestTaskLedgerRecordsIgnoreLegacyWithoutDualWriteAndSurviveReopen(t *testing.T) {
	for _, oldDocument := range []bool{false, true} {
		name := "legacy-file-only"
		if oldDocument {
			name = "legacy-file-and-document"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tasks.json")
			legacy, err := task.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Create(task.Task{Goal: "legacy file goal"}); err != nil {
				t.Fatal(err)
			}
			fileBefore, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			book, err := ledger.Open(dir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = book.Close() })
			var documentBefore []byte
			if oldDocument {
				documentBefore = []byte(`{"next_id":100,"tasks":{"99":{"id":"99","goal":"legacy document goal"}}}`)
				if err := book.Document("tasks").Save(documentBefore); err != nil {
					t.Fatal(err)
				}
			}
			assertLegacyUntouched := func(book *ledger.Ledger, wantFile []byte) {
				t.Helper()
				if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, wantFile) {
					t.Fatalf("task ledger consumed or dual-wrote legacy file: %v", err)
				}
				if _, err := os.Stat(path + ".migrated"); !os.IsNotExist(err) {
					t.Fatalf("task ledger retired legacy file: %v", err)
				}
				raw, found, err := book.Document("tasks").Load()
				if err != nil || found != oldDocument || !bytes.Equal(raw, documentBefore) {
					t.Fatalf("task ledger created or dual-wrote legacy document: found=%v err=%v", found, err)
				}
			}
			current, err := task.OpenLedger(book, path)
			if err != nil {
				t.Fatal(err)
			}
			if got := current.List(""); len(got) != 0 {
				t.Fatalf("legacy tasks were imported: %+v", got)
			}
			assertLegacyUntouched(book, fileBefore)
			tracked, err := current.Create(task.Task{Channel: "oc_1", Member: "claude", Goal: "current record"})
			if err != nil || tracked.ID != "1" {
				t.Fatalf("legacy NextID became authoritative: %+v %v", tracked, err)
			}
			if _, err := current.Begin(tracked.ID, "claude", "test-node", "test-session"); err != nil {
				t.Fatal(err)
			}
			token, err := current.ExecutionToken(tracked.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := current.BindAttempt(token, "current-attempt", "current-turn"); err != nil {
				t.Fatal(err)
			}
			title := "current metadata"
			if _, err := current.SetMeta(tracked.ID, task.MetaPatch{Title: &title}); err != nil {
				t.Fatal(err)
			}
			if err := current.SettleAttempt(tracked.ID, "current-attempt", "current-turn", time.Now().UTC(), task.OutcomeOK, task.RecoveryUsage{Tokens: task.Tokens{Input: 7, Output: 3}, Reported: true}); err != nil {
				t.Fatal(err)
			}
			want, found := current.Get(tracked.ID)
			if !found || len(want.Attempts) != 1 || want.Attempts[0].Open() || want.Budget.Tokens.Total != 10 {
				t.Fatal("fixture lacks committed current task accounting")
			}
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			assertLegacyUntouched(book, fileBefore)
			if err := book.Close(); err != nil {
				t.Fatal(err)
			}
			stray := []byte("invalid old task file must not be consulted")
			if err := os.WriteFile(path, stray, 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := ledger.Open(dir, ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			again, err := task.OpenLedger(reopened, path)
			if err != nil {
				t.Fatal(err)
			}
			got, found := again.Get(tracked.ID)
			gotJSON, err := json.Marshal(got)
			if err != nil || !found || !bytes.Equal(wantJSON, gotJSON) || len(again.List("")) != 1 || again.MetaOf(tracked.ID).Title != title {
				t.Fatalf("current record/header/history/metadata lost on reopen: %+v found=%v err=%v", got, found, err)
			}
			for _, kind := range []string{"task", "task-attempt", "task-meta", "task-store"} {
				rows, err := reopened.Bindings(t.Context(), kind)
				if err != nil || len(rows) != 1 {
					t.Fatalf("missing or duplicated %s records: count=%d err=%v", kind, len(rows), err)
				}
			}
			if err := reopened.Update(t.Context(), func(tx *ledger.Tx) error {
				head, found, err := task.GetTx(tx, tracked.ID)
				if err != nil || !found || head.Goal != want.Goal || head.Budget != want.Budget || len(head.Attempts) != 0 {
					t.Fatalf("record header missing or embeds history: %+v found=%v err=%v", head, found, err)
				}
				return task.CheckExecutionTx(tx, &token)
			}); err != nil {
				t.Fatal(err)
			}
			next, err := again.Create(task.Task{Goal: "after reopen"})
			if err != nil || next.ID != "2" {
				t.Fatalf("record NextID did not survive reopen: %+v %v", next, err)
			}
			assertLegacyUntouched(reopened, stray)
		})
	}
}

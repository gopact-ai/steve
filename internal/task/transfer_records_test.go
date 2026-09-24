package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func exportTaskRecords(t *testing.T, book *ledger.Ledger, project string) ProjectTransfer {
	t.Helper()
	var exported ProjectTransfer
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		var err error
		exported, err = ExportProjectTx(tx, project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return exported
}

func TestTaskRecordTransferFullHistoryMetadataAtomicImportAndReplay(t *testing.T) {
	s, source := taskRecordBook(t)
	root, err := s.Create(Task{Member: "parent", ProjectID: "p", Channel: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Member: "child", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	next := s.clone()
	for i := range 10000 {
		next.Tasks[child.ID].Attempts = append(next.Tasks[child.ID].Attempts, Attempt{ExecutionID: fmt.Sprint(i), StartedAt: time.Unix(0, 0).UTC(), EndedAt: time.Unix(1, 0).UTC(), Outcome: OutcomeOK})
	}
	next.Tasks[child.ID].Budget.Turns = 10000
	next.Tasks[root.ID].Budget.Turns = 10000
	if err := s.replaceData(next); err != nil {
		t.Fatal(err)
	}
	title := "full history"
	if _, err := s.SetMeta(child.ID, MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	exported := exportTaskRecords(t, source, "p")
	if len(exported.Tasks) != 2 || len(exported.Tasks[child.ID].Attempts) != 10000 || exported.Meta[child.ID].Title != title {
		t.Fatal("export omitted task history/metadata")
	}
	_, target := taskRecordBook(t)
	owner := func(tx *ledger.Tx, validateOnly bool) error {
		if validateOnly {
			return ValidateProjectImportTx(tx, exported)
		}
		return ImportProjectTx(tx, exported)
	}
	facts := ledger.TransferFacts{Bindings: map[string]map[string]json.RawMessage{"project-owner": {"p": json.RawMessage(`{"state":"importing"}`)}}}
	if err := target.ValidateImport(t.Context(), facts, nil, nil, owner); err != nil {
		t.Fatal(err)
	}
	assertEmpty := func() {
		t.Helper()
		for _, kind := range []string{taskKind, taskAttemptKind, taskMetaKind, taskStoreKind, "project-owner"} {
			if rows, err := target.Bindings(t.Context(), kind); err != nil || len(rows) != 0 {
				t.Fatalf("partial import of %s: %d %v", kind, len(rows), err)
			}
		}
	}
	assertEmpty()
	fail := errors.New("owner after tasks rejected")
	lastOwner := func(_ *ledger.Tx, validateOnly bool) error {
		if validateOnly {
			return nil
		}
		return fail
	}
	if err := target.ImportFacts(t.Context(), facts, nil, nil, owner, lastOwner); !errors.Is(err, fail) {
		t.Fatalf("late failure: %v", err)
	}
	assertEmpty()
	if err := target.ImportFacts(t.Context(), facts, nil, nil, owner); err != nil {
		t.Fatal(err)
	}
	got := exportTaskRecords(t, target, "p")
	if !reflect.DeepEqual(exported, got) {
		t.Fatal("import lost heads, historical rows, metadata or budgets")
	}
	replicated := &taskReplicator{book: target}
	if err := target.AttachReplication(replicated); err != nil {
		t.Fatal(err)
	}
	if err := target.Update(t.Context(), func(tx *ledger.Tx) error { return ImportProjectTx(tx, exported) }); err != nil {
		t.Fatal(err)
	}
	if len(replicated.payloads) != 0 {
		t.Fatal("identical task import rewrote records")
	}
	loaded, err := OpenLedger(target)
	if err != nil {
		t.Fatal(err)
	}
	created, err := loaded.Create(Task{Goal: "after import"})
	if err != nil || created.ID != "3" {
		t.Fatalf("import did not advance identity: %+v %v", created, err)
	}
	// A collision must not overwrite any of the existing accounting records.
	exported.Tasks[child.ID].Attempts[9999].ExecutionID = "changed"
	if err := target.Update(t.Context(), func(tx *ledger.Tx) error { return ImportProjectTx(tx, exported) }); err == nil {
		t.Fatal("conflicting history silently replaced imported task")
	}
	if after := exportTaskRecords(t, target, "p"); !reflect.DeepEqual(after, got) {
		t.Fatal("collision changed durable history")
	}
}

func TestTaskRecordFreezeRejectsConcurrentProjectChanges(t *testing.T) {
	for _, change := range []string{"new-task", "metadata", "attempt"} {
		t.Run(change, func(t *testing.T) {
			s, book := taskRecordBook(t)
			root, err := s.Create(Task{ProjectID: "p", Member: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			exported := exportTaskRecords(t, book, "p")
			switch change {
			case "new-task":
				_, err = s.Create(Task{ProjectID: "p", Goal: "not yet exported"})
			case "metadata":
				title := "not yet exported"
				_, err = s.SetMeta(root.ID, MetaPatch{Title: &title})
			case "attempt":
				_, err = s.Begin(root.ID, "parent", "node", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := exportTaskRecords(t, book, "p")
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := tx.PutBinding("release-marker", "p", true); err != nil {
					return err
				}
				return FreezeProjectTx(tx, exported)
			}); !errors.Is(err, ledger.ErrConflict) {
				t.Fatalf("stale bundle froze changed task facts: %v", err)
			}
			if after := exportTaskRecords(t, book, "p"); !reflect.DeepEqual(before, after) {
				t.Fatal("refused freeze partially changed tasks")
			}
			if rows, err := book.Bindings(t.Context(), "release-marker"); err != nil || len(rows) != 0 {
				t.Fatal("refused freeze did not roll back release marker", err)
			}
		})
	}
}

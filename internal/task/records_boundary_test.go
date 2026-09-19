package task

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestTaskRecordBeginTurnDestinationAndAccountingRollbackAndReopen(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = book.Close() }()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.Create(Task{Transport: "console", Channel: "chat", AnchorMessage: "old", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	input := TurnInput{Address: channel.Address{Channel: "console", Conversation: "chat", Message: "new"}, ChatID: "native", ChatType: "group", CardID: "card"}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_last_task_write BEFORE UPDATE ON bindings WHEN NEW.kind='task-store' BEGIN SELECT RAISE(ABORT,'last record rejected'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(root.ID, "worker", "node", input); err == nil {
		t.Fatal("failed final record admitted a turn")
	}
	loaded, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	stored, found := loaded.Get(root.ID)
	cached, _ := s.Get(root.ID)
	if !found || stored.AnchorMessage != "old" || stored.Budget.Turns != 0 || len(stored.Attempts) != 0 || !reflect.DeepEqual(cached, root) {
		t.Fatal("failed turn partially installed its destination or accounting")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`DROP TRIGGER fail_last_task_write`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(root.ID, "worker", "node", input); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	stored, found = loaded.Get(root.ID)
	if !found || stored.Address() != input.Address || stored.ChatID != input.ChatID || stored.ChatType != input.ChatType || stored.OpenCard != input.CardID || stored.Budget.Turns != 1 || len(stored.Attempts) != 1 {
		t.Fatalf("reopened turn lost atomic destination/admission: %+v", stored)
	}
}

func TestTaskRecordStaleStoreRejectsBudgetAndMetadataUpdates(t *testing.T) {
	for _, operation := range []string{"begin", "meta", "authorized"} {
		t.Run(operation, func(t *testing.T) {
			s, book := taskRecordBook(t)
			root, err := s.Create(Task{Channel: "chat", Member: "parent", Budget: Budget{MaxTurns: 1}})
			if err != nil {
				t.Fatal(err)
			}
			stale, err := OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Begin(root.ID, "parent", "node", ""); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "begin":
				_, err = stale.Begin(root.ID, "parent", "node", "")
			case "meta":
				title := "stale"
				_, err = stale.SetMeta(root.ID, MetaPatch{Title: &title})
			case "authorized":
				token, _ := stale.ExecutionToken(root.ID)
				_, err = stale.SpawnAuthorized(t.Context(), token, Task{Member: "child"}, func(tx *ledger.Tx) error {
					return tx.PutBinding("grant-proof", "rollback", true)
				})
			}
			if !errors.Is(err, ledger.ErrConflict) {
				t.Fatalf("stale %s: %v", operation, err)
			}
			loaded, err := OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			got, found := loaded.Get(root.ID)
			if !found || got.Budget.Turns != 1 || len(got.Attempts) != 1 || len(loaded.List("")) != 1 || loaded.MetaOf(root.ID).Title != "" {
				t.Fatal("stale write overwrote current records")
			}
			if rows, err := book.Bindings(t.Context(), "grant-proof"); err != nil || len(rows) != 0 {
				t.Fatal("stale revision did not roll back same-Tx grant", err)
			}
		})
	}
}

func TestTaskHeaderChecksIgnoreHistoryButHonorAncestorRevocation(t *testing.T) {
	s, book := taskRecordBook(t)
	root, err := s.Create(Task{Member: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(child.ID, "child", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := s.ExecutionToken(child.ID)
	// Corrupt the child's history itself: header admission must not decode it.
	if _, err := book.DB().Exec(`UPDATE bindings SET data='invalid' WHERE kind=?`, taskAttemptKind); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		got, found, err := GetTx(tx, child.ID)
		if err != nil || !found || got.ID != child.ID || len(got.Attempts) != 0 {
			t.Fatalf("header read: %+v %v", got, err)
		}
		return CheckExecutionTx(tx, &token)
	}); err != nil {
		t.Fatal("header check read historical records", err)
	}
	if _, err := OpenLedger(book, ""); err == nil {
		t.Fatal("full owner load silently ignored corrupt history")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		parent, _, err := GetTx(tx, root.ID)
		if err != nil {
			return err
		}
		parent.State = StatePaused
		return tx.PutBinding(taskKind, parent.ID, headOf(&parent))
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return CheckExecutionTx(tx, &token) }); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("paused ancestor accepted child execution: %v", err)
	}
}

func TestTaskRecordStopAndCompletionCannotMissConcurrentChild(t *testing.T) {
	for _, operation := range []string{"stop", "complete"} {
		t.Run(operation, func(t *testing.T) {
			s, book := taskRecordBook(t)
			root := idleCompletionRoot(t, s)
			stale, err := OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.Spawn(root.ID, Task{Member: "child"})
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "stop":
				_, err = stale.SetAside(root.ID, StatePaused)
			case "complete":
				_, err = stale.CompleteRoot(t.Context(), root.ID, root.Channel, func(tx *ledger.Tx, _ map[string]bool) error {
					return tx.PutBinding("completion-proof", "rollback", true)
				})
			}
			if !errors.Is(err, ledger.ErrConflict) {
				t.Fatalf("%s missed concurrent child: %v", operation, err)
			}
			loaded, err := OpenLedger(book, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, original := range []Task{root, child} {
				got, found := loaded.Get(original.ID)
				if !found || got.ExecutionEpoch != original.ExecutionEpoch || !got.State.Holds() {
					t.Fatalf("refused %s partially revoked task: %+v", operation, got)
				}
			}
			if rows, err := book.Bindings(t.Context(), "completion-proof"); err != nil || len(rows) != 0 {
				t.Fatal("stale completion persisted owner guard", err)
			}
		})
	}
}

func TestTaskRecordLoadRejectsIncompleteOrOrphanRecords(t *testing.T) {
	for _, corrupt := range []string{"control", "head", "attempt", "orphan-attempt", "orphan-meta"} {
		t.Run(corrupt, func(t *testing.T) {
			s, book := taskRecordBook(t)
			root, err := s.Create(Task{Member: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Begin(root.ID, "worker", "node", ""); err != nil {
				t.Fatal(err)
			}
			var query string
			switch corrupt {
			case "control":
				query = `DELETE FROM bindings WHERE kind='task-store'`
			case "head":
				query = `DELETE FROM bindings WHERE kind='task'`
			case "attempt":
				query = `DELETE FROM bindings WHERE kind='task-attempt'`
			case "orphan-attempt":
				query = `UPDATE bindings SET id='unknown' WHERE kind='task-attempt'`
			case "orphan-meta":
				query = `INSERT INTO bindings(kind,id,data,updated_at) VALUES('task-meta','missing','{}','2026-09-19T00:00:00Z')`
			}
			if _, err := book.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenLedger(book, ""); err == nil {
				t.Fatalf("%s silently loaded incomplete history", corrupt)
			}
		})
	}
}

func TestTaskRecordDeleteRemovesOnlySelectedTreeRows(t *testing.T) {
	s, book := taskRecordBook(t)
	root, err := s.Create(Task{Channel: "remove", Member: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.Create(Task{Channel: "keep", Member: "other"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tracked := range []Task{root, child, keep} {
		if _, err := s.Begin(tracked.ID, tracked.Member, "node", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finish(tracked.ID, OutcomeOK, Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
		title := tracked.ID
		if _, err := s.SetMeta(tracked.ID, MetaPatch{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := s.DeleteChannel("remove"); err != nil || len(ids) != 2 {
		t.Fatalf("delete=%v %v", ids, err)
	}
	loaded, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	got, found := loaded.Get(keep.ID)
	if !found || len(loaded.List("")) != 1 || len(got.Attempts) != 1 || loaded.MetaOf(keep.ID).Title != keep.ID {
		t.Fatal("delete removed unrelated records")
	}
	for _, kind := range []string{taskKind, taskAttemptKind, taskMetaKind} {
		rows, err := book.Bindings(t.Context(), kind)
		if err != nil || len(rows) != 1 {
			t.Fatalf("delete left orphan %s rows: %d %v", kind, len(rows), err)
		}
	}
}

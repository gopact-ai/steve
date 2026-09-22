package task

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestReadAccountingTxRequiresExactOriginalExecution(t *testing.T) {
	_, book := taskRecordBook(t)
	now := time.Now().UTC()
	row := attemptRecord{TaskID: "task", Index: 0, Attempt: Attempt{ExecutionID: "execution", TurnID: "turn", ExecutionEpoch: 2,
		StartedAt: now, EndedAt: now.Add(time.Second), Outcome: OutcomeOK, Tokens: Tokens{Input: 3}, Member: "worker"}}
	write := func(key string, value any) {
		t.Helper()
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return tx.PutBinding(taskAttemptKind, key, value) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return tx.PutBinding(taskKind, "task", taskHead{Task: Task{ID: "task"}, AttemptCount: 2})
	}); err != nil {
		t.Fatal(err)
	}
	write(attemptKey("task", 0), row)
	read := func(execution, turn string) (Attempt, int, bool, error) {
		var got Attempt
		var index int
		var found bool
		err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
			var err error
			got, index, found, err = ReadAccountingTx(tx, "task", execution, turn)
			return err
		})
		return got, index, found, err
	}
	got, index, found, err := read("execution", "turn")
	if err != nil || !found || index != 0 || got.ExecutionEpoch != 2 || got.Tokens.Input != 3 {
		t.Fatalf("exact original accounting: %+v %d %t %v", got, index, found, err)
	}
	for _, pair := range [][2]string{{"other", "turn"}, {"execution", "other"}} {
		if _, _, found, err := read(pair[0], pair[1]); err != nil || found {
			t.Fatalf("unrelated accounting used: %t %v", found, err)
		}
	}
	row.Index = 1
	write(attemptKey("task", 1), row)
	if _, _, _, err := read("execution", "turn"); err == nil {
		t.Fatal("duplicate accounting accepted")
	}
}

func TestReadAccountingTxRejectsMalformedOrMiskeyedRows(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{`, `{"task_id":"task","index":-1}`, `{"task_id":"task","index":0,"execution_id":"execution","turn_id":"turn"}`} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			_, book := taskRecordBook(t)
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := tx.PutBinding(taskKind, "task", taskHead{Task: Task{ID: "task"}, AttemptCount: 1}); err != nil {
					return err
				}
				return tx.PutBinding(taskAttemptKind, "wrong-key", json.RawMessage(`{}`))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := book.DB().Exec(`UPDATE bindings SET data=? WHERE kind=?`, raw, taskAttemptKind); err != nil {
				t.Fatal(err)
			}
			err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				_, _, _, err := ReadAccountingTx(tx, "task", "execution", "turn")
				return err
			})
			if err == nil {
				t.Fatal("invalid accounting became missing or authoritative")
			}
		})
	}
}

func TestAccountingReceiptLookupUsesOwnerIndex(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	diagnostic, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	rows, err := diagnostic.Query(`EXPLAIN QUERY PLAN `+accountingReceiptSQL, accountingReceiptKey("task", "execution", "turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH bindings USING INDEX bindings_task_accounting_receipt") || strings.Contains(plan, "SCAN bindings") {
		t.Fatalf("receipt lookup scans accounting history:\n%s", plan)
	}
}

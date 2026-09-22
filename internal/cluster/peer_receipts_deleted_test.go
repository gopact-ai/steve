package cluster

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// deleteReceiptTask deletes the conversation that owns the receipt's task,
// the way the owner discards a conversation.
func deleteReceiptTask(t *testing.T, book *ledger.Ledger) {
	t.Helper()
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	all := tasks.List("")
	if len(all) != 1 {
		t.Fatalf("fixture tasks = %+v", all)
	}
	if _, err := tasks.DeleteChannel(all[0].Channel); err != nil {
		t.Fatal(err)
	}
}

// The task, its accounting and its delivery went with the deleted
// conversation; nothing is left to prove or to wait for. The exact terminal
// receipt is released so the node can drop the session evidence, instead of
// every acknowledgement pass failing on the missing conversation forever.
func TestNodeReceiptProofReleasesTheReceiptOfADeletedTask(t *testing.T) {
	for _, missing := range []string{"", "delivery"} {
		t.Run("missing="+missing, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			receipt, reply := committedConsoleReceipt(t, book, "node", "")
			if missing == "delivery" {
				if _, err := book.DB().Exec(`DELETE FROM bindings WHERE kind='console-exchange' AND id=?`, reply.ExchangeID); err != nil {
					t.Fatal(err)
				}
			}
			deleteReceiptTask(t, book)
			if err := ReadNodeReceiptProof(t.Context(), book, receipt); err != nil {
				t.Fatalf("receipt of a deleted task is never released: %v", err)
			}
		})
	}
}

// Deletion is proved, not assumed from a missing header: the task must have
// been issued, and none of its records may remain.
func TestNodeReceiptProofDoesNotMistakeAPartialTaskForADeletedOne(t *testing.T) {
	for _, mode := range []string{"header-missing", "never-issued", "moved-conversation"} {
		t.Run(mode, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			receipt, _ := committedConsoleReceipt(t, book, "node", "")
			switch mode {
			case "header-missing":
				_, err = book.DB().Exec(`DELETE FROM bindings WHERE kind='task'`)
			case "never-issued":
				deleteReceiptTask(t, book)
				_, err = book.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.next_id',1) WHERE kind='task-store'`)
			case "moved-conversation":
				_, err = book.DB().Exec(`UPDATE bindings SET data=json_set(data,'$.channel','console:other') WHERE kind='task'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := ReadNodeReceiptProof(t.Context(), book, receipt); err == nil {
				t.Fatal("receipt released without proof that its task is gone")
			}
		})
	}
}

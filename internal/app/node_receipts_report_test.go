package app

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func orphanPendingReceipt(t *testing.T, book *ledger.Ledger) {
	t.Helper()
	receipt, err := nodewire.NewSessionReceipt(nodewire.SessionState{ID: "ns_" + strings.Repeat("a", 64), ContextID: "context",
		Binding: nodewire.SessionBinding{ProjectID: "p", SessionID: "conversation", TaskID: "1", AttemptID: "att-orphan", NodeID: "worker", ExecutionEpoch: 1, TaskEpoch: 1},
		Command: &nodewire.SessionCommand{ID: "turn", InputSequence: 1, State: nodewire.SessionCommandCompleted, Settled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.PutBinding(t.Context(), "attempt-node-receipt-pending", "att-orphan", receipt); err != nil {
		t.Fatal(err)
	}
}

// The reconciler runs every few seconds, and a receipt that cannot be proved
// fails the same way each time. The failure is reported when it appears and
// again only once something changed; the same failure again is not news.
func TestNodeReceiptReconcilerReportsAnUnchangedFailureOnce(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	orphanPendingReceipt(t, book)
	reconciler := &nodeReceiptReconciler{book: book, attempts: attempt.New(book),
		nodes: receiptAckFunc(func(context.Context, string, nodewire.SessionReceiptRequest) error { return nil })}
	if err := reconciler.Reconcile(t.Context()); err == nil || !strings.Contains(err.Error(), "att-orphan") {
		t.Fatalf("first failure not reported: %v", err)
	}
	for range 2 {
		if err := reconciler.Reconcile(t.Context()); err != nil {
			t.Fatalf("unchanged failure reported again: %v", err)
		}
	}
	if _, err := book.DB().Exec(`DELETE FROM bindings WHERE kind='attempt-node-receipt-pending'`); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	orphanPendingReceipt(t, book)
	if err := reconciler.Reconcile(t.Context()); err == nil {
		t.Fatal("a failure that returned after clearing was not reported")
	}
}

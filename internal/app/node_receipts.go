package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type receiptAcknowledger interface {
	AcknowledgeNodeReceipt(context.Context, string, nodewire.SessionReceiptRequest) error
}

type nodeReceiptReconciler struct {
	book      *ledger.Ledger
	attempts  *attempt.Service
	nodes     receiptAcknowledger
	authority nodewire.SessionAuthority
	after     string
}

// Reconcile examines only one page of pending receipts, never Closed history.
// Advancing the cursor past blocked receipts prevents one unknown result from
// starving later completed work. A failed RPC/index commit is retried with the
// same immutable receipt; it never grants new input or trusts a client ack.
func (r *nodeReceiptReconciler) Reconcile(ctx context.Context) error {
	pending, err := r.attempts.PendingNodeReceipts(ctx, r.after, 32)
	if err != nil {
		return err
	}
	var failures error
	for _, receipt := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.after = receipt.Binding.AttemptID
		if err := cluster.ReadNodeReceiptProof(ctx, r.book, receipt); err != nil {
			if !errors.Is(err, cluster.ErrNodeReceiptPending) {
				failures = errors.Join(failures, fmt.Errorf("read receipt %s: %w", receipt.Binding.AttemptID, err))
			}
			continue
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.nodes.AcknowledgeNodeReceipt(call, receipt.Binding.NodeID, nodewire.SessionReceiptRequest{Authority: r.authority, Receipt: receipt})
		if err == nil {
			err = r.attempts.AcknowledgeNodeReceipt(call, receipt)
		}
		cancel()
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("ack receipt %s: %w", receipt.Binding.AttemptID, err))
		}
	}
	if len(pending) < 32 {
		r.after = ""
	}
	return failures
}

func assembleNodeReceipts(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, machines fleetAssembly) {
	environment := input.Environment()
	if environment == nil || environment.ReceiptAuthorizer == nil {
		return
	}
	reconciler := &nodeReceiptReconciler{book: boot.Book(), attempts: storage.Attempts(), nodes: machines.Nodes(),
		authority: environment.PluginAuthority}
	boot.Background().Go(func(ctx context.Context) {
		runReconciler(ctx, "acknowledge original native receipts", reconciler.Reconcile)
	})
}

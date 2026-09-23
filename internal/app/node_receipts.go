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
	// reported is the failure last returned for each pending receipt, and
	// seen the receipts met since the cursor last wrapped. The reconciler
	// runs every few seconds; a receipt failing the same way again is the
	// same state, so it is returned when it appears or changes, not per pass.
	reported map[string]string
	seen     map[string]bool
}

// Reconcile examines only one page of pending receipts, never settled history.
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
		id := receipt.Binding.AttemptID
		r.after = id
		r.markSeen(id)
		if err := cluster.ReadNodeReceiptProof(ctx, r.book, receipt); err != nil {
			if errors.Is(err, cluster.ErrNodeReceiptPending) {
				r.report(id, nil)
			} else {
				failures = errors.Join(failures, r.report(id, fmt.Errorf("read receipt %s: %w", id, err)))
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
			err = fmt.Errorf("ack receipt %s: %w", id, err)
		}
		failures = errors.Join(failures, r.report(id, err))
	}
	if len(pending) < 32 {
		r.after = ""
		r.forgetUnseen()
	}
	return failures
}

// report returns err only when it differs from what was last returned for
// the receipt; nil forgets the receipt's last failure.
func (r *nodeReceiptReconciler) report(id string, err error) error {
	if err == nil {
		delete(r.reported, id)
		return nil
	}
	if r.reported[id] == err.Error() {
		return nil
	}
	if r.reported == nil {
		r.reported = map[string]string{}
	}
	r.reported[id] = err.Error()
	return err
}

func (r *nodeReceiptReconciler) markSeen(id string) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[id] = true
}

// forgetUnseen drops failures of receipts that are no longer pending once a
// full pass over the pending index is complete, so a receipt that returns
// later is reported afresh.
func (r *nodeReceiptReconciler) forgetUnseen() {
	for id := range r.reported {
		if !r.seen[id] {
			delete(r.reported, id)
		}
	}
	r.seen = nil
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

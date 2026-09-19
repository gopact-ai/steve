package lifecycle

import (
	"context"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Only the managed harness owns authenticated native evidence. Finish hooks
// supply application results, never receipt/digest authority. Collect only for
// the chat result/accounting/delivery domain supported by hub receipt proofs;
// other execution kinds retain their native evidence without pending hub GC.
func (e *Execution) nodeReceipt(ctx context.Context) *nodewire.SessionReceipt {
	if e.Record.Kind != attempt.KindChat || !e.Managed || !e.driven || !e.settled() {
		return nil
	}
	source, ok := e.Session.(harness.NodeReceiptSource)
	if !ok {
		return nil
	}
	receipt, found := source.NodeReceipt(ctx)
	if !found || receipt.Validate() != nil {
		return nil
	}
	return &receipt
}

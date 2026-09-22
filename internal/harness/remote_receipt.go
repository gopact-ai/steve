package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// NodeReceiptSource exposes only a previously authenticated node response. It
// never queries a caller for a digest or constructs proof from a client claim.
type NodeReceiptSource interface {
	NodeReceipt(context.Context) (nodewire.SessionReceipt, bool)
}

func (s *managedSession) NodeReceipt(ctx context.Context) (nodewire.SessionReceipt, bool) {
	original := s.current(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	command := state.Command
	if command == nil || !command.Settled ||
		command.State != nodewire.SessionCommandCompleted && command.State != nodewire.SessionCommandCancelled {
		return nodewire.SessionReceipt{}, false
	}
	receipt := command.Receipt
	if receipt.Validate() != nil || state.ID != s.id || receipt.SessionID != s.id ||
		receipt.ContextID != state.ContextID || receipt.Binding != original.Binding || state.Binding != original.Binding ||
		receipt.CommandID != original.CommandID || command.ID != receipt.CommandID ||
		command.InputSequence != receipt.InputSequence || original.InputSequence != 0 && original.InputSequence != receipt.InputSequence {
		return nodewire.SessionReceipt{}, false
	}
	return receipt, true
}

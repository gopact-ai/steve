package node

import (
	"github.com/gopact-ai/steve/internal/nodewire"
)

// A consumed input identity is retained only for exact session-level close
// proof. It cannot restore a pruned command's output or execution permission.
func checkCloseIdentity(req nodewire.SessionRequest, record sessionRecord) error {
	if req.Action != nodewire.SessionActionClose || record.State.ID != req.ID || record.ClusterID != req.Authority.ClusterID || record.State.Binding != req.Binding {
		return sessionError("conflict", "close belongs to another native session")
	}
	if req.Harness != "" && req.Harness != record.State.Harness {
		return sessionError("conflict", "close belongs to another harness")
	}
	if req.CommandID == "" {
		return nil
	}
	if command, exists := record.Commands[req.CommandID]; exists {
		if req.InputSequence != 0 && req.InputSequence != command.InputSequence {
			return sessionError("conflict", "close input sequence differs")
		}
		return nil
	}
	if consumed := record.Consumed; consumed != nil {
		if consumed.Validate() != nil || consumed.SessionID != record.State.ID || consumed.ContextID != record.State.ContextID || consumed.Binding != record.State.Binding || consumed.InputSequence != record.State.InputAccepted || consumed.InputSequence <= record.BindingInputStart {
			return sessionError("conflict", "consumed close identity differs")
		}
		if req.CommandID == consumed.CommandID && (req.InputSequence == 0 || req.InputSequence == consumed.InputSequence) {
			return nil
		}
	}
	if record.State.InputAccepted == record.BindingInputStart && req.InputSequence == 0 && (req.CommandID == record.OpenID || req.CommandID+"/open" == record.OpenID) {
		return nil
	}
	return sessionError("receipt_expired", "close has no exact retained command or open identity")
}

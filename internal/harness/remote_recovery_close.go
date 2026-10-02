package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// CloseRecoverySession returns the authenticated whole-process outcome of an
// exact idle close. It uses only stopping authorization, never attach/observe.
// A legacy close returning nil cannot provide this managed-session proof.
func (m *Manager) CloseRecoverySession(ctx context.Context, at Placement, id, workdir string) (nodewire.SessionState, error) {
	ctx, err := m.bindNodeSession(ctx, at, id, workdir)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	binding, bound := NodeSessionFromContext(ctx)
	if !bound || !nodewire.IsManagedSession(id) || binding.Binding.NodeID != at.Node {
		return nodewire.SessionState{}, ErrNodeSessionUnavailable
	}
	m.mu.Lock()
	transport, available := m.remote.(NodeSessionTransport)
	stopped := m.stopped
	m.mu.Unlock()
	if !available || stopped {
		return nodewire.SessionState{}, ErrNodeSessionUnavailable
	}
	request := nodewire.SessionRequest{Action: nodewire.SessionActionClose, ID: id, Authority: binding.Authority, Binding: binding.Binding, CommandID: binding.CommandID}
	state, err := transport.NodeSession(ctx, at.Node, request)
	if err != nil {
		return state, err
	}
	if state.ID != id || state.Binding != binding.Binding || state.Harness != at.Harness || !state.ProcessStopped || state.State != nodewire.SessionClosed || state.Command != nil && state.Command.ID != binding.CommandID {
		return state, unprovedKillReceipt("idle close did not prove the exact native process stopped")
	}
	return state, nil
}

package harness

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// ReconcileNodeOpen only inspects or cancels the exact admitted open command.
// It never starts a native session or reconstructs an input to replay.
func (m *Manager) ReconcileNodeOpen(ctx context.Context, at Placement, workdir string, cancelOpen bool) (nodewire.SessionState, error) {
	ctx, err := m.bindNodeSession(ctx, at, "", workdir)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	binding, bound := NodeSessionFromContext(ctx)
	if !bound || binding.Binding.NodeID == "" || binding.Binding.NodeID != at.Node || binding.CommandID == "" {
		return nodewire.SessionState{}, errors.New("original committed open identity is required")
	}
	m.mu.Lock()
	transport, ok := m.remote.(NodeSessionTransport)
	stopped := m.stopped
	m.mu.Unlock()
	if !ok || stopped {
		return nodewire.SessionState{}, ErrNodeSessionUnavailable
	}
	action := nodewire.SessionActionInspectOpen
	if cancelOpen {
		action = nodewire.SessionActionCancelOpen
	}
	req := nodewire.SessionRequest{Action: action, Authority: binding.Authority, Binding: binding.Binding, Harness: at.Harness, CommandID: binding.CommandID + "/open"}
	state, err := transport.NodeSession(ctx, at.Node, req)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	proof := state.OpenReceipt
	expected := nodewire.SessionOpenID(binding.Authority.ClusterID, at.Node, binding.Binding.AttemptID, req.CommandID, at.Harness)
	if state.ID != expected || state.Binding != req.Binding || state.Harness != at.Harness || proof == nil || proof.Action != action || proof.Authority != req.Authority || proof.CommandID != req.CommandID {
		return nodewire.SessionState{}, errors.New("node open receipt differs from the original execution or current coordinator")
	}
	if cancelOpen && (!state.ProcessStopped || state.State != "closed") {
		return nodewire.SessionState{}, ErrStopUnconfirmed
	}
	return state, nil
}

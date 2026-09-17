package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// ErrNodeOpenAbsent means the node answered and holds no record of the
// original open. It reserves its durable record before any native process
// starts, so nothing runs for that open — but a record it never received
// cannot prove the request was cancelled either, which is why sealing the
// open is still what settles it.
var ErrNodeOpenAbsent = errors.New("the node answered and has no record of the original open")

// classifyOpenRecovery keeps the node's own classification reachable, so
// recovery can tell a node that answered from a node that never replied.
func classifyOpenRecovery(err error) error {
	var coded interface{ SessionErrorCode() string }
	if errors.As(err, &coded) && coded.SessionErrorCode() == "absent" {
		return fmt.Errorf("%w: %w", ErrNodeOpenAbsent, err)
	}
	return err
}

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
		return nodewire.SessionState{}, classifyOpenRecovery(err)
	}
	proof := state.OpenReceipt
	expected := nodewire.SessionOpenID(binding.Authority.ClusterID, at.Node, binding.Binding.AttemptID, req.CommandID, at.Harness)
	if state.ID != expected || state.Binding != req.Binding || state.Harness != at.Harness || proof == nil || proof.Action != action || proof.Authority != req.Authority || proof.CommandID != req.CommandID {
		return nodewire.SessionState{}, errors.New("node open receipt differs from the original execution or current coordinator")
	}
	if cancelOpen && (!state.ProcessStopped || state.State != nodewire.SessionClosed) {
		return nodewire.SessionState{}, ErrStopUnconfirmed
	}
	return state, nil
}

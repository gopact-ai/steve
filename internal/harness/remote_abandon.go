package harness

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// AbortRetainedSession cleans up a previously abandoned execution, without
// observing or resuming it. A command receipt alone never proves its exit.
func (m *Manager) AbortRetainedSession(parent context.Context, at Placement, id, workdir string) (nodewire.SessionState, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ctx, err := m.bindNodeSession(ctx, at, id, workdir)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	binding, bound := NodeSessionFromContext(ctx)
	if !bound || !nodewire.IsManagedSession(id) || at.Node == "" || binding.Binding.NodeID != at.Node {
		return nodewire.SessionState{}, ErrNodeSessionUnavailable
	}
	m.mu.Lock()
	transport, available := m.remote.(NodeSessionTransport)
	stopped := m.stopped
	m.mu.Unlock()
	if !available || stopped {
		return nodewire.SessionState{}, ErrNodeSessionUnavailable
	}
	session := &managedSession{at: at, transport: transport, base: binding, id: id}
	request := session.request(ctx, nodewire.SessionActionAbort)
	st, err := session.call(ctx, request)
	if err != nil {
		return st, err
	}
	if !stopReceiptMatches(st, request) || !st.ProcessStopped || st.Harness != at.Harness || st.Command != nil && st.Command.ID != binding.CommandID {
		return nodewire.SessionState{}, errors.Join(ErrStopUnconfirmed, errors.New("cleanup receipt does not prove the original process stopped"))
	}
	return st, nil
}

package harness

import (
	"context"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// RetainedKiller forces the original native process to stop. Each call
// obtains a fresh receipt instead of reusing an earlier graceful stop.
type RetainedKiller interface {
	KillRetained(context.Context) (nodewire.SessionState, error)
}

func (s *managedSession) KillRetained(parent context.Context) (nodewire.SessionState, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	request := s.request(ctx, nodewire.SessionActionKill)
	state, err := s.call(ctx, request)
	if err != nil {
		return state, err
	}
	if !stopReceiptMatches(state, request) || !state.ProcessStopped {
		return state, unprovedKillReceipt("native kill receipt does not prove the original process stopped")
	}
	s.mu.Lock()
	s.stopState = state
	s.mu.Unlock()
	return state, nil
}

// KillRetainedSession addresses the recorded execution directly. Cleanup is
// still authorized when observation or plugin execution permission was revoked.
func (m *Manager) KillRetainedSession(ctx context.Context, at Placement, id, workdir string) (nodewire.SessionState, error) {
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
	state, err := session.KillRetained(ctx)
	if err != nil {
		return state, err
	}
	if state.Harness != at.Harness || state.Command != nil && state.Command.ID != binding.CommandID {
		return nodewire.SessionState{}, unprovedKillReceipt("kill receipt belongs to another original command")
	}
	return state, nil
}

// A receipt was received, but it cannot prove the requested termination.
// This is not a transport silence and must not request a node restart.
type unprovedKillReceipt string

func (e unprovedKillReceipt) Error() string            { return string(e) }
func (e unprovedKillReceipt) Unwrap() error            { return ErrStopUnconfirmed }
func (e unprovedKillReceipt) SessionErrorCode() string { return "stop_unproven" }

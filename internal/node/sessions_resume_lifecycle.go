package node

import (
	"context"
	"errors"
	"os"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// archiveStoppedSession observes process exit before a warm open can rebind the
// old execution. Pollers retain its receipts; a fresh open follows the ordinary
// stopped-context validation and exclusive handoff path.
func (s *SessionService) archiveStoppedSession(id string) error {
	s.mu.Lock()
	one := s.sessions[id]
	s.mu.Unlock()
	if one == nil {
		return nil
	}
	one.mu.Lock()
	defer one.mu.Unlock()
	// Prompt admission also takes the session lock before the service lock.
	// Recheck ownership after waiting; close may already have removed it.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sessions[id] != one {
		return nil
	}
	state := one.record.State.State
	if (state != nodewire.SessionIdle && state != nodewire.SessionInterrupted) || one.runningLocked() || one.host == nil || !one.host.AllProcessesStopped() {
		return nil
	}
	next := one.copyLocked()
	next.State.State, next.State.ProcessStopped = nodewire.SessionInterrupted, true
	for id, command := range next.Commands {
		command.ProcessStopped = true
		next.Commands[id] = command
	}
	if err := one.commitLocked(next); err != nil {
		return err
	}
	if err := s.endStoppedRuntime(next); err != nil {
		return err
	}
	delete(s.sessions, id)
	one.host.Close()
	return nil
}

func (one *ownedSession) stateAfterFailedOpen(cause error) (nodewire.SessionState, error) {
	one.host.Close()
	one.mu.Lock()
	defer one.mu.Unlock()
	next := one.copyLocked()
	next.State.ProcessStopped = one.host.AllProcessesStopped()
	err := one.commitLocked(next)
	if err == nil {
		err = one.service.endStoppedRuntime(next)
	}
	return one.stateLocked(""), errors.Join(cause, err)
}

// An explicit durable stop receipt also reconciles plugin accounting if a
// crash occurred between recording exit and releasing the runtime use. Missing
// uses need no release, including preparation that never acquired one.
func (s *SessionService) endStoppedRuntime(record sessionRecord) error {
	if !record.State.ProcessStopped || record.State.Plugin == nil {
		return nil
	}
	err := s.server.pluginStore().EndRuntimeUse(context.Background(), *record.State.Plugin, "session/"+record.State.ID)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

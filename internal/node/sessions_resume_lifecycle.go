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
func (s *SessionService) archiveStoppedSession(req nodewire.SessionRequest) error {
	id := req.ID
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
	if err := validateResumeSource(req, one.record); err != nil {
		return err
	}
	// This process opened the session, so its messaging port has not moved.
	if same, err := sameConfig(req, one.processConfigHash, processConfigHash); err != nil {
		return err
	} else if !same {
		return sessionError("forbidden", "native process was opened with a different configuration")
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
	var err error
	if next.Format == 1 {
		err = errors.Join(one.commitLocked(next), s.endStoppedRuntime(next))
	} else if err = one.commitLocked(next); err == nil {
		err = s.endStoppedRuntime(one.record)
	}
	if err != nil {
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
	if next.Format == 1 {
		err := errors.Join(one.commitLocked(next), one.service.endStoppedRuntime(next))
		return one.stateLocked(""), errors.Join(cause, err)
	}
	if err := one.commitLocked(next); err != nil {
		return one.stateLocked(""), errors.Join(cause, err)
	}
	return one.stateLocked(""), errors.Join(cause, one.service.endStoppedRuntime(one.record))
}

// An explicit durable stop receipt also reconciles plugin accounting if a
// crash occurred between recording exit and releasing the runtime use. Missing
// uses need no release, including preparation that never acquired one.
func (s *SessionService) endStoppedRuntime(record sessionRecord) error {
	if !record.State.ProcessStopped || record.State.Plugin == nil {
		return nil
	}
	if !sessionTerminalsStopped(record) {
		return sessionError("uncertain", "plugin runtime use has outstanding terminal cleanup")
	}
	if err := validateSessionTerminals(record); err != nil {
		return err
	}
	if record.Format == 2 {
		// Terminal-aware ownership requires a durable aggregate stop, not
		// just a caller's proposed transport stop. Legacy physical-stop
		// cleanup remains independent of receipt persistence.
		durable, exists, err := s.readRecord(record.State.ID)
		if err != nil {
			return err
		}
		if !exists || durable.Format != 2 || durable.State.Sequence != record.State.Sequence ||
			!durable.State.ProcessStopped || !sessionTerminalsStopped(durable) ||
			durable.Process != record.Process || durable.State.Plugin == nil ||
			durable.State.Plugin.ID != record.State.Plugin.ID {
			return sessionError("uncertain", "plugin runtime use lacks its original durable aggregate stop")
		}
		want, err := record.State.Plugin.Selection.Hash()
		if err != nil {
			return err
		}
		actual, err := durable.State.Plugin.Selection.Hash()
		if err != nil {
			return err
		}
		if actual != want {
			return sessionError("uncertain", "plugin runtime use differs from its original durable selection")
		}
		record = durable
	}
	err := s.server.pluginStore().EndRuntimeUse(context.Background(), *record.State.Plugin, "session/"+record.State.ID)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

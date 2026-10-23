package node

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

// A session whose receipt journal refused a write latches that failure and
// refuses every later input: what its agent does next can no longer be
// recorded. The agent itself may already have exited, or keep running with
// nobody able to hear from it. Either way the coordinator still owes the
// original execution a stop, so a failed owner keeps answering the requests
// that settle it: observation reads the durable record, and a stop ends the
// native process and records its exit on the durable record — which accepts
// that fact even when it refused the write that failed. A stop that cannot be
// proved keeps answering the original failure, so the execution stays
// isolated.
func (s *SessionService) doFailed(ctx context.Context, one *ownedSession, failure error, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	clusterID, old, binding := one.record.ClusterID, one.record.Authority, one.record.State.Binding
	one.mu.Unlock()
	a := req.Authority
	if a.ClusterID != clusterID || a.CoordinatorEpoch < old.CoordinatorEpoch || a.WriterGeneration < old.WriterGeneration ||
		a.CoordinatorEpoch == old.CoordinatorEpoch && a.CoordinatorNodeID != old.CoordinatorNodeID {
		return nodewire.SessionState{}, sessionError("forbidden", "stale coordinator activation")
	}
	switch req.Action {
	case nodewire.SessionActionAttach, nodewire.SessionActionSettings:
		return s.durableState(req)
	case nodewire.SessionActionCancel, nodewire.SessionActionAbort, nodewire.SessionActionKill, nodewire.SessionActionClose:
		// Only the execution the session is bound to may end its process.
		//
		// Cancel here ends the whole native process, not just the current
		// turn as it does on a healthy owner: a failed owner can't record
		// what a surviving agent does next, so the only stop it can prove
		// is the process's exit. The session is left Interrupted rather
		// than Closed, as after a node restart.
		if req.Binding != binding {
			return nodewire.SessionState{}, sessionError("conflict", "session belongs to another execution")
		}
		if err := s.retireFailed(ctx, one, failure, req.Action != nodewire.SessionActionCancel); err != nil {
			return nodewire.SessionState{}, err
		}
		return s.closedState(req)
	default:
		return nodewire.SessionState{}, failure
	}
}

// durableState is the last receipt the journal accepted for req's command.
func (s *SessionService) durableState(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	store, err := s.recordsStore()
	if err != nil {
		return nodewire.SessionState{}, err
	}
	record, exists, err := store.read(req.ID, req.CommandID)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !exists {
		return nodewire.SessionState{}, sessionError("unavailable", "node session receipt cannot be read")
	}
	if record.ClusterID != req.Authority.ClusterID {
		return nodewire.SessionState{}, sessionError("forbidden", "session belongs to another cluster")
	}
	if record.State.Binding != req.Binding {
		return nodewire.SessionState{}, sessionError("conflict", "session belongs to another execution")
	}
	if err := checkSessionReceipt(req, record); err != nil {
		return nodewire.SessionState{}, err
	}
	return (&ownedSession{record: record}).stateLocked(req.CommandID), nil
}

// retireFailed ends a failed owner's native process and records the stop.
// The recorded process group is the proof, as after a node restart. When
// the group can't be checked at all (none was recorded, or this node can't
// track groups) the host that started the agent may prove it instead,
// having seen every process it started exit, unless retained terminals
// need their own proof. A group check that ran and found members gets no
// fallback. Once the stop is durable the owner is released, so later
// requests are answered from the durable record; moving the session and
// its commands out of their running states is attempted, not required,
// since the journal may refuse it the way it refused the write that failed.
func (s *SessionService) retireFailed(ctx context.Context, one *ownedSession, failure error, closed bool) error {
	one.mu.Lock()
	id, host := one.record.State.ID, one.host
	openCancel, openDone, runDone := one.openCancel, one.openDone, one.runDone
	one.mu.Unlock()
	if openCancel != nil {
		openCancel()
	}
	hostStopped := false
	if host != nil {
		if err := host.Kill(ctx); err != nil {
			slog.Warn("steve-node: failed session's agent did not confirm its kill", "session", id, "error", err)
		}
		for _, done := range []<-chan struct{}{openDone, runDone} {
			if done == nil {
				continue
			}
			select {
			case <-done:
			case <-ctx.Done():
				return errors.Join(failure, killFailure(ctx.Err()))
			}
		}
		hostStopped = host.AllProcessesStopped()
	}
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	record, exists, err := s.readRecord(id)
	if err != nil {
		return errors.Join(failure, err)
	}
	if !exists {
		return errors.Join(failure, sessionError("stop_unproven", "original process identity is unavailable"))
	}
	fresh := &ownedSession{service: s, record: record, changed: make(chan struct{})}
	if !record.State.ProcessStopped {
		deadline := time.Now().Add(recordedGroupWithin)
		if err := s.endRecordedGroup(fresh, deadline); err != nil {
			if !hostStopped || len(record.Terminals) != 0 || fresh.failure != nil || !groupUncheckable(record.Process, err) {
				return errors.Join(failure, killFailure(err))
			}
			if err := fresh.commitRecordedStop(deadline); err != nil {
				return errors.Join(failure, err)
			}
		}
	}
	// The stop is durable now. Each further transition is its own commit,
	// so one the journal refuses doesn't take the others with it.
	settle := func(what string, change func(*sessionRecord) bool) {
		next := fresh.copyLocked()
		if !change(&next) {
			return
		}
		probe := &ownedSession{service: s, record: fresh.record, changed: make(chan struct{})}
		if err := probe.commitLocked(next); err != nil {
			slog.Warn("steve-node: failed session's "+what+" not recorded", "session", id, "error", err)
			return
		}
		fresh = probe
	}
	settle("final state", func(next *sessionRecord) bool {
		changed := false
		switch {
		case next.State.State == nodewire.SessionClosed:
		case closed:
			next.State.State, changed = nodewire.SessionClosed, true
		case next.State.State != nodewire.SessionInterrupted:
			next.State.State, changed = nodewire.SessionInterrupted, true
		}
		for cid, command := range next.Commands {
			if command.State.Active() {
				command.State = nodewire.SessionCommandUncertain
				command.Error, command.ErrorCode = "node session journal failed; the native process was ended", nodewire.SessionErrorFailed
				command.Settled = false
				next.Commands[cid] = command
				changed = true
			}
		}
		return changed
	})
	settle("pending questions", func(next *sessionRecord) bool {
		changed := false
		for i := range next.State.Questions {
			if next.State.Questions[i].State == nodewire.SessionQuestionPending {
				next.State.Questions[i].State = nodewire.SessionQuestionInterrupted
				changed = true
			}
		}
		return changed
	})
	if err := s.endStoppedRuntime(fresh.record); err != nil {
		slog.Error("steve-node: plugin shutdown receipt failed", "session", id, "error", err)
	}
	if plugin := fresh.record.State.Plugin; plugin != nil {
		if err := s.server.pluginRuntimePool().Drop(plugin.ID); err != nil {
			slog.Error("steve-node: plugin runtime release failed", "session", id, "error", err)
		}
	}
	s.mu.Lock()
	if s.sessions[id] == one {
		delete(s.sessions, id)
	}
	delete(s.unverifiedProcesses, id)
	if s.retiredFailed == nil {
		s.retiredFailed = map[string]bool{}
	}
	s.retiredFailed[id] = true
	s.mu.Unlock()
	one.mu.Lock()
	close(one.changed)
	one.changed = make(chan struct{})
	one.mu.Unlock()
	slog.Warn("steve-node: ended the native process of a session whose journal failed", "session", id, "cause", failure)
	return nil
}

// groupUncheckable reports whether ending the recorded group failed because
// it couldn't be checked, rather than because members were seen: no group
// was recorded, or this node can't prove one.
func groupUncheckable(process sessionProcess, err error) bool {
	return process.Identity == (procgroup.Identity{}) || errors.Is(err, procgroup.ErrUnsupported) || errors.Is(err, procgroup.ErrUnproven)
}

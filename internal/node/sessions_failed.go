package node

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
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
	record := one.record
	one.mu.Unlock()
	binding := record.State.Binding
	if err := checkRecordedAuthority(record, req.Authority); err != nil {
		return nodewire.SessionState{}, err
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
		// A stop the receipt check after it would refuse must not end the
		// process first: retiring is irreversible.
		if err := s.checkFailedStop(one, req); err != nil {
			return nodewire.SessionState{}, err
		}
		if err := s.retireFailed(ctx, one, failure, req.Action != nodewire.SessionActionCancel); err != nil {
			return nodewire.SessionState{}, err
		}
		return s.closedState(req)
	default:
		return nodewire.SessionState{}, failure
	}
}

// checkRecordedAuthority is admitLocked's coordinator check without the
// commit that records a newer activation, which a failed journal refuses.
func checkRecordedAuthority(record sessionRecord, a nodewire.SessionAuthority) error {
	old := record.Authority
	if a.ClusterID != record.ClusterID || a.CoordinatorEpoch < old.CoordinatorEpoch || a.WriterGeneration < old.WriterGeneration ||
		a.CoordinatorEpoch == old.CoordinatorEpoch && a.CoordinatorNodeID != old.CoordinatorNodeID {
		return sessionError("forbidden", "stale coordinator activation")
	}
	return nil
}

// reconcileFailedOpen answers an open recovery for a failed owner whose
// open identity the caller has checked: the hub has no confirmed session ID
// for it, so this, not a session stop, is how it asks for the stop.
func (s *SessionService) reconcileFailedOpen(ctx context.Context, one *ownedSession, failure error, id string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	record := one.record
	one.mu.Unlock()
	if err := checkRecordedAuthority(record, req.Authority); err != nil {
		return nodewire.SessionState{}, err
	}
	if req.Action == nodewire.SessionActionCancelOpen || req.Action == nodewire.SessionActionKill {
		if err := s.retireFailed(ctx, one, failure, true); err != nil {
			return nodewire.SessionState{}, err
		}
	}
	store, err := s.recordsStore()
	if err != nil {
		return nodewire.SessionState{}, err
	}
	durable, exists, err := store.read(id, "")
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !exists {
		return nodewire.SessionState{}, sessionError("unavailable", "node session receipt cannot be read")
	}
	return openRecoveryReceipt(s.retiredOpenState(id, durable), req, false), nil
}

// retiredOpenState is a record's state as an open recovery reports it. A
// session a failed owner retired is closed whether or not the journal took
// the Closed state after its recorded stop.
func (s *SessionService) retiredOpenState(id string, record sessionRecord) nodewire.SessionState {
	state := (&ownedSession{record: record}).stateLocked("")
	s.mu.Lock()
	retired := s.retiredFailed[id]
	s.mu.Unlock()
	if retired && state.ProcessStopped {
		state.State = nodewire.SessionClosed
	}
	return state
}

// checkFailedStop refuses, before anything is ended, a stop the healthy
// owner would refuse and one whose receipt closedState would then refuse:
// another command while one runs, and a close or receipt identity the
// durable record doesn't hold.
func (s *SessionService) checkFailedStop(one *ownedSession, req nodewire.SessionRequest) error {
	one.mu.Lock()
	current, running := one.record.CurrentCommand, one.runningLocked()
	one.mu.Unlock()
	if req.Action != nodewire.SessionActionClose && req.CommandID != "" && req.CommandID != current && running {
		return sessionError("conflict", "stop targets another command")
	}
	store, err := s.recordsStore()
	if err != nil {
		return err
	}
	selected := req.CommandID
	if req.Action == nodewire.SessionActionClose {
		selected = ""
	}
	record, exists, err := store.read(req.ID, selected)
	if err != nil {
		return err
	}
	if !exists {
		return sessionError("unavailable", "node session receipt cannot be read")
	}
	if record.ClusterID != req.Authority.ClusterID {
		return sessionError("forbidden", "session belongs to another cluster")
	}
	if record.State.Binding != req.Binding {
		return sessionError("conflict", "session belongs to another execution")
	}
	if req.Action == nodewire.SessionActionClose {
		return checkCloseIdentity(req, record)
	}
	return checkSessionReceipt(req, record)
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
	id := one.record.State.ID
	one.mu.Unlock()
	hostStopped, err := s.endFailedHost(ctx, one)
	if err != nil {
		return errors.Join(failure, err)
	}
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	fresh, err := s.recordFailedStop(id, hostStopped)
	if err != nil {
		return errors.Join(failure, err)
	}
	fresh = s.settleRetired(fresh, closed)
	// The owner is kept until its plugin use is settled too: once released,
	// nothing visits the use again before a restart. A later stop finds the
	// stop recorded and retries just this.
	if err := s.endStoppedRuntime(fresh.record); err != nil {
		return errors.Join(failure, err)
	}
	if plugin := fresh.record.State.Plugin; plugin != nil {
		if err := s.server.pluginRuntimePool().Drop(plugin.ID); err != nil {
			return errors.Join(failure, err)
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

// endFailedHost kills a failed owner's host and reports whether the host
// saw every process it started exit. Its workspace callbacks must drain
// whatever proves the exit: one still running can write after the agent's
// group is gone, so a stop recorded before it ends would release the
// workspace too early.
func (s *SessionService) endFailedHost(ctx context.Context, one *ownedSession) (bool, error) {
	one.mu.Lock()
	id, host := one.record.State.ID, one.host
	openCancel, openDone, runDone := one.openCancel, one.openDone, one.runDone
	one.mu.Unlock()
	if openCancel != nil {
		openCancel()
	}
	if host == nil {
		return false, nil
	}
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
			return false, killFailure(ctx.Err())
		}
	}
	// The host's own record of the exit can trail the kill by the moment
	// its watcher takes to see the group empty, and a cancelled callback
	// takes a moment to return.
	drained := s.hostDrained
	if drained == nil {
		drained = (*acphost.Host).CallbacksDrained
	}
	stopped, idle := host.AllProcessesStopped(), drained(host)
	for wait := time.Now().Add(recordedGroupWithin); (!stopped || !idle) && time.Now().Before(wait); {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return false, killFailure(ctx.Err())
		}
		stopped, idle = host.AllProcessesStopped(), drained(host)
	}
	if !idle {
		return false, sessionError("stop_unproven", "a workspace callback of the original agent is still running")
	}
	return stopped, nil
}

// recordFailedStop proves and records the stop on a fresh read of the
// session's record, under settleMu.
func (s *SessionService) recordFailedStop(id string, hostStopped bool) (*ownedSession, error) {
	record, exists, err := s.readRecord(id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, sessionError("stop_unproven", "original process identity is unavailable")
	}
	fresh := &ownedSession{service: s, record: record, changed: make(chan struct{})}
	if record.State.ProcessStopped {
		return fresh, nil
	}
	deadline := time.Now().Add(recordedGroupWithin)
	err = s.endRecordedGroup(fresh, deadline)
	if err == nil {
		return fresh, nil
	}
	if !hostStopped || len(record.Terminals) != 0 || fresh.failure != nil || !groupUncheckable(record.Process, err) {
		return nil, killFailure(err)
	}
	if err := fresh.commitRecordedStop(deadline); err != nil {
		return nil, err
	}
	return fresh, nil
}

// settleRetired moves a stopped record's session, commands and questions
// out of their running states. Each is its own commit, so one the journal
// refuses doesn't take the others with it; none is required.
func (s *SessionService) settleRetired(fresh *ownedSession, closed bool) *ownedSession {
	id := fresh.record.State.ID
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
	return fresh
}

// groupUncheckable reports whether ending the recorded group failed because
// it couldn't be checked, rather than because members were seen: no group
// was recorded, or this node can't prove one.
func groupUncheckable(process sessionProcess, err error) bool {
	return process.Identity == (procgroup.Identity{}) || errors.Is(err, procgroup.ErrUnsupported) || errors.Is(err, procgroup.ErrUnproven)
}

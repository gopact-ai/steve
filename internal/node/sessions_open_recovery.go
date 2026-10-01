package node

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// reconcileOpen never creates an agent. The same mutex that reserves an open
// serializes its absence check and cancellation tombstone with late requests.
func (s *SessionService) reconcileOpen(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if req.ID != "" || !sessionNameValid(req.CommandID) || !sessionNameValid(req.Harness) {
		return nodewire.SessionState{}, sessionError("invalid", "open recovery requires the original open command and harness")
	}
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nodewire.SessionState{}, sessionError("closed", "node session service is closed")
	}
	one := s.sessions[id]
	if one == nil {
		state, retry, err := s.reconcileRecordedOpenLocked(id, req)
		s.mu.Unlock()
		// Only an admitted cancellation tries again to end what a killed
		// node left: a refused one must not signal anything.
		if req.Action == nodewire.SessionActionKill && retry {
			check := req
			check.ID = id
			check.CommandID = ""
			state, err := s.killRecorded(check)
			return openRecoveryReceipt(state, req, false), err
		}
		if retry && s.settleUnverified(id) {
			return s.reconcileOpen(ctx, req)
		}
		return state, err
	}
	s.mu.Unlock()
	one.mu.Lock()
	if one.record.OpenID != req.CommandID || one.record.State.Harness != req.Harness || one.record.State.Binding != req.Binding {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("forbidden", "open receipt belongs to another execution")
	}
	check := req
	check.ID, check.Action = id, nodewire.SessionActionAttach
	if err := one.admitLocked(check); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	state := one.stateLocked("")
	one.mu.Unlock()
	if req.Action == nodewire.SessionActionKill {
		check.Action, check.CommandID = nodewire.SessionActionKill, ""
		state, err := one.kill(ctx, check)
		return openRecoveryReceipt(state, req, false), err
	}
	if req.Action == nodewire.SessionActionCancelOpen {
		check.Action, check.CommandID = nodewire.SessionActionAbort, ""
		var err error
		state, err = one.stop(ctx, check)
		if err != nil {
			return nodewire.SessionState{}, err
		}
	}
	return openRecoveryReceipt(state, req, false), nil
}

// reconcileRecordedOpenLocked answers an open recovery from the open's
// record alone; the caller holds s.mu. retry reports an admitted
// cancellation refused only because the record's process stop is not
// confirmed.
func (s *SessionService) reconcileRecordedOpenLocked(id string, req nodewire.SessionRequest) (_ nodewire.SessionState, retry bool, _ error) {
	record, exists, err := s.readRecord(id)
	if err != nil {
		return nodewire.SessionState{}, false, err
	}
	if !exists {
		if req.Action != nodewire.SessionActionCancelOpen && req.Action != nodewire.SessionActionKill {
			return nodewire.SessionState{}, false, sessionError("absent", "original open is not recorded; absence alone does not confirm cancellation")
		}
		cancelled := &ownedSession{service: s, changed: make(chan struct{})}
		record = sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenCancelled: true, CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}, State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: req.Harness, State: nodewire.SessionClosed, ProcessStopped: true, Questions: []nodewire.SessionQuestion{}}}
		if err := cancelled.commitLocked(record); err != nil {
			return nodewire.SessionState{}, false, err
		}
		record = cancelled.record
	}
	if record.ClusterID != req.Authority.ClusterID || record.OpenID != req.CommandID || record.State.Binding != req.Binding || record.State.Harness != req.Harness {
		return nodewire.SessionState{}, false, sessionError("forbidden", "open receipt belongs to another execution")
	}
	one := &ownedSession{service: s, record: record, changed: make(chan struct{})}
	check := req
	check.ID, check.Action = id, nodewire.SessionActionAttach
	if err := one.admitLocked(check); err != nil {
		return nodewire.SessionState{}, false, err
	}
	if (req.Action == nodewire.SessionActionCancelOpen || req.Action == nodewire.SessionActionKill) && !one.record.State.ProcessStopped {
		return nodewire.SessionState{}, true, sessionError("uncertain", "original native process stop is not confirmed")
	}
	if req.Action == nodewire.SessionActionCancelOpen || req.Action == nodewire.SessionActionKill {
		next := one.copyLocked()
		var saveErr error
		if next.State.State != nodewire.SessionClosed {
			next.State.State = nodewire.SessionClosed
			saveErr = one.commitLocked(next)
		}
		if err := errors.Join(saveErr, s.endStoppedRuntime(next)); err != nil {
			return nodewire.SessionState{}, false, err
		}
	}
	return openRecoveryReceipt(one.stateLocked(""), req, record.OpenCancelled), false, nil
}

func openRecoveryReceipt(state nodewire.SessionState, req nodewire.SessionRequest, cancelled bool) nodewire.SessionState {
	state.OpenReceipt = &nodewire.SessionOpenReceipt{Action: req.Action, Authority: req.Authority, CommandID: req.CommandID, CancelledBeforeOpen: cancelled}
	return state
}

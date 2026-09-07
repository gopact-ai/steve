package node

import (
	"context"

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
		defer s.mu.Unlock()
		record, exists, err := s.readRecord(id)
		if err != nil {
			return nodewire.SessionState{}, err
		}
		if !exists {
			if req.Action != "cancel-open" {
				return nodewire.SessionState{}, sessionError("unavailable", "original open is not recorded; absence alone does not confirm cancellation")
			}
			one = &ownedSession{service: s, changed: make(chan struct{})}
			record = sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenCancelled: true, CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}, State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: req.Harness, State: "closed", ProcessStopped: true, Questions: []nodewire.SessionQuestion{}}}
			if err := one.commitLocked(record); err != nil {
				return nodewire.SessionState{}, err
			}
			record = one.record
		}
		if record.ClusterID != req.Authority.ClusterID || record.OpenID != req.CommandID || record.State.Binding != req.Binding || record.State.Harness != req.Harness {
			return nodewire.SessionState{}, sessionError("forbidden", "open receipt belongs to another execution")
		}
		one = &ownedSession{service: s, record: record, changed: make(chan struct{})}
		check := req
		check.ID, check.Action = id, "attach"
		if err := one.admitLocked(check); err != nil {
			return nodewire.SessionState{}, err
		}
		if req.Action == "cancel-open" && !one.record.State.ProcessStopped {
			return nodewire.SessionState{}, sessionError("uncertain", "original native process stop is not confirmed")
		}
		if req.Action == "cancel-open" && one.record.State.State != "closed" {
			next := one.copyLocked()
			next.State.State = "closed"
			if err := one.commitLocked(next); err != nil {
				return nodewire.SessionState{}, err
			}
		}
		return openRecoveryReceipt(one.stateLocked(""), req, record.OpenCancelled), nil
	}
	s.mu.Unlock()
	one.mu.Lock()
	if one.record.OpenID != req.CommandID || one.record.State.Harness != req.Harness || one.record.State.Binding != req.Binding {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("forbidden", "open receipt belongs to another execution")
	}
	check := req
	check.ID, check.Action = id, "attach"
	if err := one.admitLocked(check); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	state := one.stateLocked("")
	one.mu.Unlock()
	if req.Action == "cancel-open" {
		check.Action, check.CommandID = "abort", ""
		var err error
		state, err = one.stop(ctx, check)
		if err != nil {
			return nodewire.SessionState{}, err
		}
	}
	return openRecoveryReceipt(state, req, false), nil
}

func openRecoveryReceipt(state nodewire.SessionState, req nodewire.SessionRequest, cancelled bool) nodewire.SessionState {
	state.OpenReceipt = &nodewire.SessionOpenReceipt{Action: req.Action, Authority: req.Authority, CommandID: req.CommandID, CancelledBeforeOpen: cancelled}
	return state
}

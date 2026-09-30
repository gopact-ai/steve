package node

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
	"time"
)

func killFailure(err error) error {
	code := "stop_unproven"
	switch {
	case errors.Is(err, procgroup.ErrUnsupported):
		code = "stop_unsupported"
	case errors.Is(err, procgroup.ErrRunning):
		code = "stop_running"
	}
	return sessionError(code, err.Error())
}

func (one *ownedSession) kill(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	if err := one.admitLocked(req); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	if req.CommandID == "" {
		req.CommandID = one.record.CurrentCommand
	}
	if one.runningLocked() && req.CommandID != one.record.CurrentCommand {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("conflict", "kill targets another command")
	}
	host, generation := one.host, one.record.Generation
	if host == nil {
		one.mu.Unlock()
		return one.state(req.CommandID), sessionError("unavailable", "original native process cannot be contacted")
	}
	next := one.copyLocked()
	next.State.State = nodewire.SessionClosing
	if err := one.commitLocked(next); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	openCancel, openDone, runDone := one.openCancel, one.openDone, one.runDone
	one.mu.Unlock()
	if openCancel != nil {
		openCancel()
	}
	if err := host.Kill(ctx); err != nil && !host.AllProcessesStopped() {
		return one.state(req.CommandID), killFailure(err)
	}
	for _, done := range []<-chan struct{}{openDone, runDone} {
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return one.state(req.CommandID), killFailure(ctx.Err())
			}
		}
	}
	state, err := one.settleStop(req, host, generation)
	if errors.Is(err, acphost.ErrStopUnconfirmed) {
		err = killFailure(procgroup.ErrUnproven)
	}
	return state, err
}

func (s *SessionService) killRecorded(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	record, exists, err := s.readRecord(req.ID)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !exists {
		return nodewire.SessionState{}, sessionError("stop_unproven", "original process identity is unavailable")
	}
	if record.ClusterID != req.Authority.ClusterID || record.State.Binding != req.Binding {
		return nodewire.SessionState{}, sessionError("conflict", "recorded session belongs to another execution")
	}
	if err := checkSessionReceipt(req, record); err != nil {
		return nodewire.SessionState{}, err
	}
	one := &ownedSession{service: s, record: record, changed: make(chan struct{})}
	if !record.State.ProcessStopped {
		if err := s.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err != nil {
			return record.State, killFailure(err)
		}
	}
	s.mu.Lock()
	delete(s.unverifiedProcesses, req.ID)
	s.mu.Unlock()
	if err := s.endStoppedRuntime(one.record); err != nil {
		return one.record.State, err
	}
	return one.state(req.CommandID), nil
}

package node

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

func (s *SessionService) inspectOpen(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return s.reconcileOpen(ctx, req)
}

func (s *SessionService) cancelOpen(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return s.reconcileOpen(ctx, req)
}

func (s *SessionService) probeCapabilities(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	spec, ok := s.server.conf().Harnesses[req.Harness]
	if !ok {
		return nodewire.SessionState{}, sessionError("unavailable", "harness is not registered on this node")
	}
	broker, _ := permission.New(permission.PolicyRead)
	cfg := s.hostConfig(req.Harness, spec, broker)
	key := capabilityKey(cfg)
	if cached, ok := s.cachedCapabilities(req.Harness, key); ok {
		return nodewire.SessionState{Binding: req.Binding, Harness: req.Harness, SupportsHTTPMCP: cached.supportsHTTPMCP}, nil
	}
	// Starting the adapter once says what its agent accepts; the answer
	// is kept so the next turn does not pay for a process of its own.
	host := acphost.New(cfg)
	defer host.Close()
	supported, err := host.SupportsHTTPMCP(ctx)
	if err == nil {
		s.rememberCapabilities(req.Harness, key, supported)
	}
	return nodewire.SessionState{Binding: req.Binding, Harness: req.Harness, SupportsHTTPMCP: supported}, err
}

func (one *ownedSession) open(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stateForRequest(req)
}

func (one *ownedSession) attach(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stateForRequest(req)
}

func (one *ownedSession) settings(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stateForRequest(req)
}

func (one *ownedSession) poll(ctx context.Context, principal string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if req.WaitMS < 0 || req.WaitMS > 25000 {
		return nodewire.SessionState{}, sessionError("invalid", "poll wait exceeds limit")
	}
	one.mu.Lock()
	changed, sequence := one.changed, one.record.State.Sequence
	one.mu.Unlock()
	if sequence <= req.After && req.WaitMS > 0 {
		timer := time.NewTimer(time.Duration(req.WaitMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
			return nodewire.SessionState{}, ctx.Err()
		case <-one.service.ctx.Done():
			return nodewire.SessionState{}, sessionError("closed", "node session service closed")
		}
	}
	if err := one.service.authorize(ctx, principal, req); err != nil {
		return nodewire.SessionState{}, err
	}
	return one.stateForRequest(req)
}

func (one *ownedSession) option(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	if err := one.admitLocked(req); err != nil {
		one.mu.Unlock()
		return nodewire.SessionState{}, err
	}
	host, id, generation := one.host, one.record.UpstreamID, one.record.Generation
	// A selector can be set while the agent is answering. Approval mode is
	// why: an owner who stops approving each command means now, not after
	// this turn. The agent takes it on the same connection the turn runs
	// on, so only an idle session parks in configuring — a running one
	// keeps its state and its receipt.
	status := one.record.State.State
	answering := status == nodewire.SessionRunning || one.runningLocked()
	settled := status == nodewire.SessionIdle && !answering
	if host == nil || status.Unavailable() || !(answering || settled) {
		one.mu.Unlock()
		return nodewire.SessionState{}, sessionError("busy", "settings require a live session")
	}
	if settled {
		next := one.copyLocked()
		next.State.State = nodewire.SessionConfiguring
		if err := one.commitLocked(next); err != nil {
			one.mu.Unlock()
			return nodewire.SessionState{}, err
		}
	}
	one.mu.Unlock()
	optionErr := host.SetOption(ctx, acp.SessionID(id), generation, acp.SessionConfigID(req.OptionID), req.OptionValue)
	one.mu.Lock()
	next := one.copyLocked()
	if next.State.State == nodewire.SessionConfiguring {
		next.State.State = nodewire.SessionIdle
		if host.ProcessStopped(generation) {
			next.State.State = nodewire.SessionInterrupted
			next.State.ProcessStopped = true
		}
	}
	saveErr := one.commitLocked(next)
	one.mu.Unlock()
	return one.state(req.CommandID), errors.Join(optionErr, saveErr)
}

func (one *ownedSession) cancel(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stop(ctx, req)
}

func (one *ownedSession) abort(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stop(ctx, req)
}

func (one *ownedSession) close(ctx context.Context, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	return one.stop(ctx, req)
}

package sshconnect

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/i18n"
)

// MemberRestart names a specific local restart or upgrade, not the latest
// operation that happens to have run on the same machine.
type MemberRestart struct {
	NodeID    string `json:"node_id"`
	RequestID string `json:"request_id"`
	PlanID    string `json:"plan_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Phase     string `json:"phase,omitempty"`
}

type RestartClaim func(context.Context, string, string) (bool, error)

type RestartVerification func(context.Context, string, string) error

// BeginMemberRestart reserves the shared machine slot before the one-shot
// claim. It never starts a replacement when the claim's reply was lost.
func (s *Service) BeginMemberRestart(ctx context.Context, node, request string, claim RestartClaim, verify RestartVerification) (MemberRestart, error) {
	ctx, text := s.speak(ctx)
	backend, ok := s.backend.(RestartBackend)
	if !ok {
		return MemberRestart{}, restartUnsupported(text)
	}
	if request == "" || claim == nil || verify == nil || !backend.Knows(ctx, node) {
		return MemberRestart{}, errors.New("invalid member restart")
	}
	if _, err := backend.RestartTarget(ctx, node); err != nil {
		return MemberRestart{}, err
	}
	s.mu.Lock()
	if prior, found := s.memberRestarts[request]; found {
		s.mu.Unlock()
		if prior.NodeID != node {
			return MemberRestart{}, errors.New("restart request names another machine")
		}
		return s.MemberRestartStatus(ctx, node, request, prior.PlanID, prior.Kind)
	}
	if s.closed {
		s.mu.Unlock()
		return MemberRestart{}, errors.New("restart service is closed")
	}
	var stored *storedPlan
	kind := "restart"
	fresh := false
	switch {
	case s.running(s.upgrades, node):
		stored = s.plans[s.upgrades[node]]
		kind = "upgrade"
	case s.running(s.restarts, node):
		stored = s.plans[s.restarts[node]]
	default:
		var failure *StepError
		stored, failure = s.claimRestart(text, node, false)
		if failure != nil {
			s.mu.Unlock()
			return MemberRestart{}, failure
		}
		fresh = true
	}
	op := MemberRestart{NodeID: node, RequestID: request, PlanID: stored.plan.ID, Kind: kind, State: "claiming"}
	if s.memberRestarts == nil {
		s.memberRestarts = map[string]MemberRestart{}
	}
	s.memberRestarts[request] = op
	// Close waits for claims as well as started scripts; no child outlives it.
	s.autoRuns.Add(1)
	s.mu.Unlock()
	claimed, err := claim(ctx, op.PlanID, kind)
	if err != nil || !claimed {
		if fresh {
			s.settle(stored, InstallResult{PlanID: op.PlanID, NodeID: node, Status: "needs_attention"}, err)
		}
		s.mu.Lock()
		op.State = "lost"
		s.memberRestarts[request] = op
		s.mu.Unlock()
		s.autoRuns.Done()
		if err == nil {
			err = errors.New("restart was already claimed; inspect the original operation")
		}
		return op, err
	}
	s.mu.Lock()
	op.State = "running"
	s.memberRestarts[request] = op
	s.mu.Unlock()
	if fresh {
		runCtx := i18n.WithLocale(s.autoCtx, text.Locale())
		go func() {
			defer s.autoRuns.Done()
			_, _ = s.runMemberRestartChecked(runCtx, backend, stored, node, func(checkCtx context.Context) error {
				s.mu.Lock()
				same := !s.closed && s.plans[op.PlanID] == stored && s.restarts[node] == op.PlanID && stored.running
				s.mu.Unlock()
				if !same {
					return errors.New("member restart no longer holds its machine slot")
				}
				return verify(checkCtx, op.PlanID, kind)
			})
		}()
	} else {
		s.autoRuns.Done()
	}
	return s.MemberRestartStatus(ctx, node, request, op.PlanID, kind)
}

func (s *Service) MemberRestartStatus(ctx context.Context, node, request, plan, kind string) (MemberRestart, error) {
	if err := ctx.Err(); err != nil {
		return MemberRestart{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lost := MemberRestart{NodeID: node, RequestID: request, PlanID: plan, Kind: kind, State: "lost"}
	op, found := s.memberRestarts[request]
	if !found || op.NodeID != node || op.PlanID != plan || op.Kind != kind || op.State == "lost" {
		return lost, nil
	}
	if op.State == "claiming" {
		return op, nil
	}
	stored := s.plans[plan]
	if stored == nil || stored.plan.Request.Name != node {
		return lost, nil
	}
	op.Phase = stored.result.Phase
	switch {
	case stored.running:
		op.State = "running"
	case stored.result.Connected && stored.result.Status == "connected":
		op.State = "connected"
	default:
		op.State = "failed"
	}
	return op, nil
}

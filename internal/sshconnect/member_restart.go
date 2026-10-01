package sshconnect

import (
	"context"
	"errors"
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

func (s *Service) BeginMemberRestart(ctx context.Context, node, request string, claim RestartClaim) (MemberRestart, error) {
	return MemberRestart{}, errors.New("member restart is unavailable")
}
func (s *Service) MemberRestartStatus(ctx context.Context, node, request, plan, kind string) (MemberRestart, error) {
	return MemberRestart{NodeID: node, RequestID: request, PlanID: plan, Kind: kind, State: "lost"}, nil
}

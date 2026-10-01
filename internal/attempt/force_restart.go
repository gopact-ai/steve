package attempt

import (
	"context"
	"errors"
	"time"
)

// ForceRestart is the latest durable restart of one machine. Its ID is never
// reused: a delayed request for a replaced operation is refused.
type ForceRestart struct {
	ID          string    `json:"id"`
	ClusterID   string    `json:"cluster_id"`
	NodeID      string    `json:"node_id"`
	Holder      string    `json:"holder"`
	By          string    `json:"by"`
	RequestedAt time.Time `json:"requested_at"`
	ClaimedAt   time.Time `json:"claimed_at,omitempty"`
	PlanID      string    `json:"plan_id,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
}

var ErrForceRestartChanged = errors.New("member restart identity changed")

func (s *Service) BeginForceRestart(ctx context.Context, id string, revision uint64, clusterID, holder string) (ForceRestart, bool, error) {
	return ForceRestart{}, false, errors.New("member restart is unavailable")
}
func (s *Service) ForceRestart(ctx context.Context, node string) (ForceRestart, bool, error) {
	return ForceRestart{}, false, nil
}
func (s *Service) ClaimForceRestart(ctx context.Context, request ForceRestart, plan, kind string) (bool, error) {
	return false, errors.New("member restart is unavailable")
}
func (s *Service) FinishForceRestart(ctx context.Context, request ForceRestart, outcome string) error {
	return errors.New("member restart is unavailable")
}

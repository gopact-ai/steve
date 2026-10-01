package cluster

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type restartCandidate struct {
	holder string
	memberRestartResponse
}

// The target cannot hold the SSH slot for stopping itself. Every other enrolled
// candidate is queried, not just the first member with a link or an online view.
func (m memberRestarts) discover(ctx context.Context, node, by string, state coordination.State, authority nodewire.SessionAuthority) ([]restartCandidate, error) {
	service, err := m.peer.restartService()
	if err != nil {
		return nil, err
	}
	local, err := service.RestartStatus(ctx, node)
	if err != nil {
		return nil, err
	}
	answer := memberRestartResponse{Restartable: local.Restartable, MembershipRevision: state.Revision}
	if local.Activity != nil {
		answer.Operation = *local.Activity
	}
	out := []restartCandidate{{holder: m.peer.Config.NodeID, memberRestartResponse: answer}}
	ids := make([]string, 0, len(state.Members))
	for id := range state.Members {
		if id != m.peer.Config.NodeID && id != node {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// Leave time for the caller to retain an explicit incomplete outcome. The
	// persisted phase clock, not this per-pass deadline, bounds repeated probes.
	deadline := time.Now().Add(19 * time.Second)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline.Add(time.Second)) {
		deadline = caller.Add(-time.Second)
	}
	probeBudget, cancelBudget := context.WithDeadline(ctx, deadline)
	defer cancelBudget()
	tried := make(map[string]bool, len(ids))
	complete := true
	for range len(ids) {
		if probeBudget.Err() != nil {
			complete = false
			break
		}
		id := m.discovery.next(ids, tried)
		query := restartQuery(node, "", "", "", by, authority)
		var response memberRestartResponse
		probe, cancel := context.WithTimeout(probeBudget, 3*time.Second)
		err := m.peer.peerJSON(probe, state.Members[id], http.MethodGet, clusterMemberRestartPath+"?"+query.Encode(), nil, &response)
		cancel()
		if err != nil {
			complete = false
			continue
		}
		if response.MembershipRevision != state.Revision || response.Operation.PlanID != "" && (response.Operation.NodeID != node || response.Operation.State != sshconnect.MemberRestartRunning || response.Operation.Kind != "restart" && response.Operation.Kind != "upgrade") {
			return nil, MemberRestartError{"restart_discovery_changed"}
		}
		if state.Removing[id] && (response.Restartable || response.Operation.PlanID != "") {
			return nil, MemberRestartError{"restart_discovery_changed"}
		}
		out = append(out, restartCandidate{holder: id, memberRestartResponse: response})
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !complete {
		return nil, MemberRestartError{"restart_discovery_incomplete"}
	}
	current, _, err := m.authority(ctx, node, by)
	if err != nil {
		return nil, err
	}
	if current.Revision != state.Revision {
		return nil, MemberRestartError{"restart_discovery_changed"}
	}
	return out, nil
}

func chooseRestartTarget(cluster string, revision uint64, candidates []restartCandidate) (RestartTarget, error) {
	var links, active []restartCandidate
	for _, candidate := range candidates {
		if candidate.Restartable {
			links = append(links, candidate)
		}
		if candidate.Operation.PlanID != "" {
			active = append(active, candidate)
		}
	}
	if len(active) > 1 {
		return RestartTarget{}, MemberRestartError{"restart_conflicting_plans"}
	}
	selection := attempt.ForceRestartSelection{MembershipRevision: revision}
	if len(active) == 1 {
		selection.JoinPlanID, selection.JoinKind = active[0].Operation.PlanID, active[0].Operation.Kind
		return RestartTarget{ClusterID: cluster, Holder: active[0].holder, Selection: selection}, nil
	}
	if len(links) > 1 {
		return RestartTarget{}, MemberRestartError{"restart_ambiguous_holder"}
	}
	if len(links) == 0 {
		return RestartTarget{}, MemberRestartError{"restart_no_holder"}
	}
	return RestartTarget{ClusterID: cluster, Holder: links[0].holder, Selection: selection}, nil
}

// An already claimed fresh plan is the only expected activity at execution.
// Re-read the candidate view after preflight; a cached unique link is not enough.
func (m memberRestarts) verifyRestartSelection(ctx context.Context, op attempt.ForceRestart, ownPlan, ownKind string) error {
	state, authority, err := m.authority(ctx, op.NodeID, op.By)
	if err != nil {
		return err
	}
	if op.Selection.MembershipRevision == 0 || state.Revision != op.Selection.MembershipRevision {
		return MemberRestartError{"restart_discovery_changed"}
	}
	candidates, err := m.discover(ctx, op.NodeID, op.By, state, authority)
	if err != nil {
		return err
	}
	if ownPlan != "" {
		matched := false
		for i := range candidates {
			candidate := &candidates[i]
			if candidate.holder == op.Holder && candidate.Operation.PlanID == ownPlan && candidate.Operation.Kind == ownKind {
				candidate.Operation.PlanID = ""
				matched = true
			}
		}
		if !matched {
			return MemberRestartError{"restart_status_lost"}
		}
	}
	target, err := chooseRestartTarget(op.ClusterID, state.Revision, candidates)
	if err != nil {
		return err
	}
	if target.Holder != op.Holder || target.Selection != op.Selection {
		return MemberRestartError{"restart_discovery_changed"}
	}
	return nil
}

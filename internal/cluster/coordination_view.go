package cluster

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
)

func coordinationNodes(ctx context.Context, state coordination.State, localID string, local Status, probe func(context.Context, coordination.Member) (coordination.Progress, error)) []consoleapi.CoordinatorNode {
	ids := make([]string, 0, len(state.Members))
	for id := range state.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	nodes := make([]consoleapi.CoordinatorNode, len(ids))
	var group sync.WaitGroup
	if len(ids) == 0 {
		return nodes
	}
	jobs := make(chan int)
	workerCount := 4
	if len(ids) < workerCount {
		workerCount = len(ids)
	}
	for range workerCount {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				nodes[index] = coordinationNode(ctx, state, localID, local, state.Members[ids[index]], probe)
			}
		}()
	}
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	group.Wait()
	return nodes
}

func coordinationNode(ctx context.Context, state coordination.State, localID string, local Status, member coordination.Member, probe func(context.Context, coordination.Member) (coordination.Progress, error)) consoleapi.CoordinatorNode {
	text := i18n.FromContext(ctx)
	id := member.NodeID
	item := consoleapi.CoordinatorNode{ID: id, Name: member.Name, Local: id == localID, Voter: state.Voters[id] != "", AutoEligible: member.AutoEligible}
	if item.Name == "" {
		item.Name = id
	}
	var progress coordination.Progress
	var probeErr error
	offline := text.T(i18n.ClusterNodeOffline)
	if item.Local {
		progress = local.Progress()
		if !local.Healthy {
			probeErr = coordination.ErrUnavailable
			offline = text.T(i18n.ClusterLocalServiceStopped)
		}
	} else {
		// Probes travel through the authenticated SSH/mux route. A one-second
		// budget makes healthy but busy peers flap offline. This is only the
		// reachability observation; readiness still requires both sync fences.
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		progress, probeErr = probe(probeCtx, member)
		cancel()
	}
	item.Online = probeErr == nil
	_, pendingVote := state.PendingVotes[id]
	item.Ready = item.Online && progress.AppliedIndex >= state.AppliedIndex && progress.AppVersion >= state.AppVersion && state.IsActiveReplica(id) && !pendingVote
	if !item.Online {
		item.Reason = offline
	} else if !state.IsActiveReplica(id) {
		item.Reason = text.T(i18n.ClusterMembershipPending)
	} else if pendingVote {
		item.Reason = text.T(i18n.ClusterVotePending)
	} else if !item.Ready {
		item.Reason = text.T(i18n.ClusterSyncing)
	}
	return item
}

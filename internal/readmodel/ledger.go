package readmodel

import (
	"context"
	"sort"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
)

// Ledger adapts the attempt and artifact services to the read model.
type Ledger struct {
	Attempts  *attempt.Service
	Artifacts *artifact.Store
	Projects  *project.Store
}

func (l Ledger) LiveAttempts(ctx context.Context) []Attempt {
	if l.Attempts == nil {
		return nil
	}
	live, err := l.Attempts.Live(ctx)
	if err != nil {
		return nil
	}
	out := make([]Attempt, 0, len(live))
	for _, r := range live {
		leases := make([]string, 0, len(r.Leases))
		for _, lease := range r.Leases {
			leases = append(leases, lease.Key)
		}
		out = append(out, Attempt{
			ID: r.ID, Kind: string(r.Kind), State: string(r.State), TaskID: r.TaskID, Project: r.Project,
			Agent: r.Agent, Node: r.Node, Scope: string(r.Scope), Workspace: r.Workspace.Path, Leases: leases, StartedAt: r.StartedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (l Ledger) RecentLandings(ctx context.Context) []Landing {
	if l.Artifacts == nil || l.Projects == nil {
		return nil
	}
	projects, err := l.Projects.List(ctx)
	if err != nil {
		return nil
	}
	var out []Landing
	for _, p := range projects {
		landings, err := l.Artifacts.Landings(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, land := range landings {
			at := land.EndedAt
			if at.IsZero() {
				at = land.StartedAt
			}
			out = append(out, Landing{ID: land.ID, Project: land.Project, Artifact: land.Artifact, State: land.State, Paths: len(land.Paths), Error: land.Error, At: at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

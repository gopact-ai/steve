package readmodel

import (
	"context"
	"github.com/gopact-ai/steve/internal/ledger"
	"sort"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/project"
)

// Ledger adapts the attempt and artifact services to the read model.
type Ledger struct {
	Book      *ledger.Ledger
	Attempts  *attempt.Service
	Artifacts *artifact.Store
	Projects  *project.Store
	Intents   *intent.Service
}

// ClosedAttempts are the attempts that reached a terminal state.
func (l Ledger) ClosedAttempts(ctx context.Context) ([]attempt.Record, error) {
	if l.Attempts == nil {
		return nil, nil
	}
	return l.Attempts.Closed(ctx)
}

// Events pages the ledger journal newest first.
func (l Ledger) Events(ctx context.Context, before int64, limit int) ([]ledger.Event, error) {
	if l.Book == nil {
		return nil, nil
	}
	return l.Book.RecentEvents(ctx, before, limit)
}

// ProjectList lists the projects on record.
func (l Ledger) ProjectList(ctx context.Context) []project.Project {
	if l.Projects == nil {
		return nil
	}
	all, err := l.Projects.List(ctx)
	if err != nil {
		return nil
	}
	return all
}

// Facts gathers the ledger's other records. Every list is non-nil so the
// JSON says "none" rather than "unknown".
func (l Ledger) Facts(ctx context.Context) Facts {
	f := Facts{Reservations: []Reservation{}, Attestations: []Attestation{}, Replicas: []Replica{}, Disclosures: []Disclosure{}, Effects: []Effect{}, Grants: []Grant{}}
	if l.Attempts != nil {
		if rs, err := l.Attempts.Reservations(ctx); err == nil {
			for _, r := range rs {
				f.Reservations = append(f.Reservations, Reservation{ID: r.ID, Endpoint: r.Endpoint, For: r.For, Region: r.Lease.Region, ExpiresAt: r.Lease.ExpiresAt})
			}
		}
	}
	if l.Artifacts != nil {
		if as, err := l.Artifacts.Attestations(ctx, ""); err == nil {
			for _, a := range tail(as, 30) {
				f.Attestations = append(f.Attestations, Attestation{Artifact: a.Artifact, Step: a.Step, Kind: a.Kind, Verifier: a.Verifier, Verdict: a.Verdict, Detail: a.Detail, Attempt: a.By, At: a.At})
			}
		}
		if rs, err := l.Artifacts.Replicas(ctx, ""); err == nil {
			for _, r := range tail(rs, 40) {
				f.Replicas = append(f.Replicas, Replica{Artifact: r.Artifact, Node: r.Node, Generation: r.Generation, State: r.State, Note: r.Note, At: r.At})
			}
		}
	}
	if l.Projects != nil {
		if ds, err := l.Projects.PendingDisclosures(ctx); err == nil {
			for _, d := range ds {
				f.Disclosures = append(f.Disclosures, Disclosure{ID: d.ID, Project: d.Project, TaskID: d.TaskID, Requester: d.Requester, Bytes: d.Bytes, At: d.ProposedAt})
			}
		}
		if gs, err := l.Projects.Grants(ctx, ""); err == nil {
			for _, g := range gs {
				f.Grants = append(f.Grants, Grant{Project: g.Project, Principal: g.Principal, Role: string(g.Role), By: g.By})
			}
		}
	}
	if l.Intents != nil {
		if is, err := l.Intents.Unresolved(ctx); err == nil {
			for _, it := range is {
				f.Effects = append(f.Effects, Effect{ID: it.ID, Tool: it.Tool, TaskID: it.TaskID, Attempt: it.AttemptID, Error: it.Error, At: it.At})
			}
		}
	}
	return f
}

func tail[T any](in []T, n int) []T {
	if len(in) > n {
		return in[len(in)-n:]
	}
	return in
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
			Requires: r.Requires, Admission: r.Admission,
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

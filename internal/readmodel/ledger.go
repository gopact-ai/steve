package readmodel

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/steve/internal/ledger"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/project"
)

// Ledger adapts the attempt and artifact services to the read model.
type Ledger struct {
	Book     *ledger.Ledger
	Attempts interface {
		Closed(context.Context) ([]attempt.Record, error)
		Live(context.Context) ([]attempt.Record, error)
		Reservations(context.Context) ([]attempt.Reservation, error)
	}
	Artifacts interface {
		Attestations(context.Context, string) ([]artifact.Attestation, error)
		Replicas(context.Context, string) ([]artifact.Replica, error)
		Landings(context.Context, string) ([]artifact.Landing, error)
	}
	Projects interface {
		List(context.Context) ([]project.Project, error)
		PendingDisclosures(context.Context) ([]project.DisclosureRequest, error)
		Grants(context.Context, string) ([]project.Grant, error)
	}
	Intents interface {
		PendingResolution(context.Context) ([]intent.Intent, error)
	}
}

// ClosedAttempts are the attempts that reached a terminal state.
func (l Ledger) ClosedAttempts(ctx context.Context) ([]attempt.Record, error) {
	if l.Attempts == nil {
		return nil, errors.New("attempt source is not configured")
	}
	return l.Attempts.Closed(ctx)
}

// Events pages the ledger journal newest first.
func (l Ledger) Events(ctx context.Context, before int64, limit int) ([]ledger.Event, error) {
	if l.Book == nil {
		return nil, errors.New("ledger event source is not configured")
	}
	return l.Book.RecentEvents(ctx, before, limit)
}

// ProjectList lists the projects on record.
func (l Ledger) ProjectList(ctx context.Context) ([]project.Project, error) {
	if l.Projects == nil {
		return nil, errors.New("project source is not configured")
	}
	return l.Projects.List(ctx)
}

// Facts preserves successful records even when a subquery is incomplete.
// Inbox completeness is kept separately from unrelated fact-group failures.
func (l Ledger) Facts(ctx context.Context) (Facts, error) {
	var f Facts
	normalizeFacts(&f)
	var failures []error
	var disclosuresKnown, effectsKnown bool
	failed := func(name string, err error) {
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
	}
	if l.Attempts != nil {
		rs, err := l.Attempts.Reservations(ctx)
		failed("reservations", err)
		for _, r := range rs {
			f.Reservations = append(f.Reservations, Reservation{ID: r.ID, Endpoint: r.Endpoint, For: r.For, Region: r.Lease.Region, ExpiresAt: r.Lease.ExpiresAt})
		}
	} else {
		failed("reservations", errors.New("attempt source is not configured"))
	}
	if l.Artifacts != nil {
		as, err := l.Artifacts.Attestations(ctx, "")
		failed("attestations", err)
		for _, a := range tail(as, 30) {
			f.Attestations = append(f.Attestations, Attestation{Artifact: a.Artifact, Step: a.Step, Kind: a.Kind, Verifier: a.Verifier, Verdict: a.Verdict, Detail: a.Detail, Attempt: a.By, At: a.At})
		}
		rs, err := l.Artifacts.Replicas(ctx, "")
		failed("replicas", err)
		for _, r := range tail(rs, 40) {
			f.Replicas = append(f.Replicas, Replica{Artifact: r.Artifact, Node: r.Node, Generation: r.Generation, State: r.State, Note: r.Note, At: r.At})
		}
	} else {
		failed("attestations and replicas", errors.New("artifact source is not configured"))
	}
	if l.Projects != nil {
		ds, err := l.Projects.PendingDisclosures(ctx)
		disclosuresKnown = err == nil
		failed("disclosures", err)
		for _, d := range ds {
			f.Disclosures = append(f.Disclosures, Disclosure{ID: d.ID, Project: d.Project, TaskID: d.TaskID, Requester: d.Requester, Bytes: d.Bytes, At: d.ProposedAt})
		}
		gs, err := l.Projects.Grants(ctx, "")
		failed("grants", err)
		for _, g := range gs {
			f.Grants = append(f.Grants, Grant{Project: g.Project, Principal: g.Principal, Role: string(g.Role), By: g.By})
		}
	} else {
		failed("disclosures and grants", errors.New("project source is not configured"))
	}
	if l.Intents != nil {
		is, err := l.Intents.PendingResolution(ctx)
		effectsKnown = err == nil
		failed("effects", err)
		for _, it := range is {
			detail := it.Error
			if detail == "" {
				detail = "执行结果尚未确认"
			}
			f.Effects = append(f.Effects, Effect{ID: it.ID, Tool: it.Tool, TaskID: it.TaskID, Attempt: it.AttemptID, Error: detail, At: it.At})
		}
	} else {
		failed("effects", errors.New("intent source is not configured"))
	}
	f.attentionKnown = disclosuresKnown && effectsKnown
	return f, errors.Join(failures...)
}

func tail[T any](in []T, n int) []T {
	if len(in) > n {
		return in[len(in)-n:]
	}
	return in
}

func (l Ledger) LiveAttempts(ctx context.Context) ([]Attempt, error) {
	if l.Attempts == nil {
		return nil, errors.New("attempt source is not configured")
	}
	live, err := l.Attempts.Live(ctx)
	out := make([]Attempt, 0, len(live))
	for _, r := range live {
		leases := make([]string, 0, len(r.Leases))
		for _, lease := range r.Leases {
			leases = append(leases, lease.Key)
		}
		out = append(out, Attempt{
			ID: r.ID, Kind: string(r.Kind), State: string(r.State), TaskID: r.TaskID, Project: r.Project,
			Agent: r.Agent, Node: r.Node, Scope: string(r.Scope), Workspace: r.Workspace.Path, Leases: leases, StartedAt: r.StartedAt,
			Requires: r.Requires, Admission: r.Admission, Unsettled: r.Unsettled, Error: r.Error,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, err
}

func (l Ledger) RecentLandings(ctx context.Context) ([]Landing, error) {
	if l.Artifacts == nil || l.Projects == nil {
		return nil, errors.New("artifact or project source is not configured")
	}
	projects, err := l.Projects.List(ctx)
	var out []Landing
	var failures []error
	if err != nil {
		failures = append(failures, fmt.Errorf("projects: %w", err))
	}
	for _, p := range projects {
		landings, err := l.Artifacts.Landings(ctx, p.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("project %s: %w", p.ID, err))
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
	return out, errors.Join(failures...)
}

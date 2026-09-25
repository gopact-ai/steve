package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"github.com/gopact-ai/steve/internal/datalevel"
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
		UsageSamples(context.Context) ([]attempt.UsageSample, error)
		Live(context.Context) ([]attempt.Record, error)
		Reservations(context.Context) ([]attempt.Reservation, error)
	}
	Artifacts interface {
		RecentAttestations(context.Context) ([]artifact.Attestation, error)
		RecentReplicas(context.Context) ([]artifact.Replica, error)
		RecentLandings(context.Context) ([]artifact.Landing, error)
		AllStuck(context.Context) ([]artifact.Stuck, error)
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

// UsageSamples are the spend fields of the attempts that reached a terminal
// state.
func (l Ledger) UsageSamples(ctx context.Context) ([]attempt.UsageSample, error) {
	if l.Attempts == nil {
		return nil, errors.New("attempt source is not configured")
	}
	return l.Attempts.UsageSamples(ctx)
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
		as, err := l.Artifacts.RecentAttestations(ctx)
		failed("attestations", err)
		for _, a := range as {
			f.Attestations = append(f.Attestations, Attestation{Artifact: a.Artifact, Step: a.Step, Kind: a.Kind, Verifier: a.Verifier, Verdict: a.Verdict, Detail: a.Detail, Attempt: a.By, At: a.At})
		}
		rs, err := l.Artifacts.RecentReplicas(ctx)
		failed("replicas", err)
		for _, r := range rs {
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
	if l.Artifacts == nil {
		return nil, errors.New("artifact source is not configured")
	}
	landings, err := l.Artifacts.RecentLandings(ctx)
	var out []Landing
	for _, land := range landings {
		at := land.EndedAt
		if at.IsZero() {
			at = land.StartedAt
		}
		entry := Landing{ID: land.ID, Project: land.Project, Artifact: land.Artifact, State: land.State, Paths: len(land.Paths), Error: land.Error, At: at}
		if land.State == artifact.LandMergeConflicted {
			entry.Files, entry.Resolvable = land.Paths, land.Conflict != ""
		}
		out = append(out, entry)
	}
	return out, err
}

// Conflicts is what is stopped on a merge conflict across every project,
// oldest first. It is read from the pending queue rather than from recent
// landings, so a conflict that has been waiting a long time is still
// reported: a list of blockers that quietly drops the oldest one is worse
// than no list.
func (l Ledger) Conflicts(ctx context.Context) ([]Conflict, error) {
	if l.Artifacts == nil {
		return nil, errors.New("artifact source is not configured")
	}
	stuck, err := l.Artifacts.AllStuck(ctx)
	if err != nil {
		return nil, err
	}
	homes := map[string]project.Project{}
	if l.Projects != nil {
		projects, listErr := l.Projects.List(ctx)
		if listErr != nil {
			err = errors.Join(err, fmt.Errorf("projects: %w", listErr))
		}
		for _, p := range projects {
			homes[p.ID] = p
		}
	}
	out := make([]Conflict, 0, len(stuck))
	for _, item := range stuck {
		p, known := homes[item.Project]
		entry := Conflict{
			Project: item.Project, Artifact: item.Artifact, Landing: item.Landing,
			Files: item.Paths, Resolvable: item.Resolvable(), Reason: item.Reason, Attempt: item.Attempt, At: item.At,
		}
		if known {
			entry.Node = p.Home.Node
			// A sealed project keeps its data at home, so the hub cannot
			// check the half-merged tree out for a person to edit.
			entry.Editable = item.Resolvable() && !(p.Level == datalevel.Sealed && p.Home.Node != "")
		}
		out = append(out, entry)
	}
	return out, err
}

// Observations keeps observations in the ledger's bindings, one binding
// each, so a write replicates only the observations it saves. The binding
// id is the observation's number, zero-padded so that the ids sort in
// number order and forgetting the oldest is one range delete.
type Observations struct{ Book *ledger.Ledger }

const observationKind = "observation"

func observationID(n uint64) string { return fmt.Sprintf("%020d", n) }

func (o Observations) Load(ctx context.Context) ([]Observation, uint64, error) {
	raw, err := o.Book.Bindings(ctx, observationKind)
	if err != nil {
		return nil, 0, err
	}
	type numbered struct {
		n   uint64
		obs Observation
	}
	// A record that cannot be read costs that record only: failing the
	// load would keep every later observation from being saved. One whose
	// id is a number still holds that number, so none is reused.
	kept := make([]numbered, 0, len(raw))
	var last uint64
	for id, data := range raw {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || id != observationID(n) {
			slog.Warn("readmodel: skipped an observation record whose id is not an observation number", "id", id)
			continue
		}
		last = max(last, n)
		var obs Observation
		if err := json.Unmarshal(data, &obs); err != nil {
			slog.Warn("readmodel: skipped an unreadable observation record", "id", id, "error", err.Error())
			continue
		}
		kept = append(kept, numbered{n, obs})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].n < kept[j].n })
	list := make([]Observation, len(kept))
	for i, k := range kept {
		list[i] = k.obs
	}
	return list, last, nil
}

func (o Observations) Save(ctx context.Context, first uint64, list []Observation, keep uint64) error {
	return o.Book.Update(ctx, func(tx *ledger.Tx) error {
		for i, obs := range list {
			if err := tx.PutBinding(observationKind, observationID(first+uint64(i)), obs); err != nil {
				return err
			}
		}
		if keep > 1 {
			if _, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id < ?`, observationKind, observationID(keep)); err != nil {
				return err
			}
		}
		return nil
	})
}

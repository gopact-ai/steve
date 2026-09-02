package readmodel

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

// Snapshot reads every source once. It never fails: a source that is not
// wired simply contributes nothing, because a dashboard that goes blank when
// one subsystem is off is worse than one that shows the rest.
func (m *Model) Snapshot(ctx context.Context) Snapshot {
	snap := Snapshot{At: time.Now(), Hub: m.src.Hub}
	if m.src.Nodes != nil {
		snap.Nodes = nodes(m.src.Nodes.Statuses())
	}
	if m.src.Roster != nil {
		for _, c := range m.src.Roster.All(ctx) {
			snap.Agents = append(snap.Agents, Agent{
				ID: c.Agent.ID, Node: c.Node, Harness: c.Harness,
				Model: c.Agent.Model, Eligible: c.Eligible, Why: c.Why,
				Requires: c.Agent.Requires, Level: string(c.Level.OrDefault()), Slots: c.Slots, Region: c.Region,
			})
		}
	}
	planByTask := map[string]plan.Plan{}
	if m.src.Plans != nil {
		for _, p := range m.src.Plans.List() {
			snap.Plans = append(snap.Plans, convertPlan(p))
			if p.TaskID != "" {
				planByTask[p.TaskID] = p
			}
		}
	}
	if m.src.Tasks != nil {
		snap.Tasks = tasks(m.src.Tasks.List(""), planByTask)
	}
	// Absence is a fact too: every list is present, empty or not, so a
	// renderer never has to guess whether "none" meant "not asked".
	snap.Attempts, snap.Landings = []Attempt{}, []Landing{}
	snap.Facts = Facts{Reservations: []Reservation{}, Attestations: []Attestation{}, Replicas: []Replica{}, Disclosures: []Disclosure{}, Effects: []Effect{}, Grants: []Grant{}}
	if m.src.Ledger != nil {
		if live := m.src.Ledger.LiveAttempts(ctx); live != nil {
			snap.Attempts = live
		}
		if recent := m.src.Ledger.RecentLandings(ctx); recent != nil {
			snap.Landings = recent
		}
		snap.Facts = m.src.Ledger.Facts(ctx)
	}
	return snap
}

func nodes(statuses []node.Status) []Node {
	out := make([]Node, 0, len(statuses))
	for _, s := range statuses {
		n := Node{
			Name: s.Name, Addr: s.Addr, Up: s.Up, Since: s.Since,
			OS: s.Advert.OS, Arch: s.Advert.Arch,
			Capabilities: s.Advert.Capabilities, LastError: s.LastError, Level: s.Level, Region: s.Region,
		}
		for _, h := range s.Advert.Harnesses {
			n.Harnesses = append(n.Harnesses, Harness{ID: h.ID, Models: h.Models, Missing: h.Missing})
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// tasks converts the task list and links parents to children, so a renderer
// can draw the tree without walking the list twice.
func tasks(list []task.Task, plans map[string]plan.Plan) []Task {
	children := map[string][]string{}
	for _, t := range list {
		if t.Parent != "" {
			children[t.Parent] = append(children[t.Parent], t.ID)
		}
	}
	out := make([]Task, 0, len(list))
	for _, t := range list {
		item := Task{
			ID: t.ID, Goal: t.Goal, State: string(t.State), Member: t.Member,
			NodeID: t.Node, Parent: t.Parent, Children: children[t.ID],
			Turns: t.Budget.Turns, MaxTurns: t.Budget.MaxTurns,
			Elapsed:   t.Budget.Elapsed.Round(time.Second).String(),
			MaxElapse: t.Budget.MaxElapsed.Round(time.Minute).String(),
			UpdatedAt: t.UpdatedAt,
		}
		if p, ok := plans[t.ID]; ok {
			item.PlanID = p.ID
		}
		out = append(out, item)
	}
	return out
}

func convertPlan(p plan.Plan) Plan {
	out := Plan{ID: p.ID, TaskID: p.TaskID, Rev: p.Rev, Goal: p.Goal, By: p.By, Because: p.Because}
	for _, s := range p.Steps {
		step := Step{
			ID: s.ID, Goal: s.Goal, State: string(s.State), Agent: s.Agent,
			Needs: s.Needs, Merge: s.Merge, Requires: s.Requires, Attempts: s.Attempts,
		}
		if s.Verify != nil {
			step.Verify = string(s.Verify.Kind)
		}
		if s.Result != nil {
			step.Node = s.Result.Node
			step.Error = s.Result.Error
		}
		out.Steps = append(out.Steps, step)
	}
	return out
}

// DescribeContext renders an assembled payload for the read model. It is a
// summary rather than the raw text: the point is to answer "what did that
// agent see?" at a glance, with the size that matters for the budget.
func DescribeContext(goal string, ancestry []string, refs []plan.Ref, findings []plan.Finding,
	facts []string, turnsLeft, bytes int) *StepContext {
	out := &StepContext{
		Goal: goal, Ancestry: ancestry, Facts: facts, TurnsLeft: turnsLeft, Bytes: bytes,
	}
	for _, ref := range refs {
		out.Refs = append(out.Refs, fmt.Sprintf("%s:%s", ref.Kind, ref.Value))
	}
	for _, f := range findings {
		out.Findings = append(out.Findings, f.Text)
	}
	return out
}

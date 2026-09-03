package readmodel

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/nodewire"
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
	if m.src.HubAdvert != nil {
		snap.Hub.Advert = m.src.HubAdvert()
		snap.Hub.Version = snap.Hub.Advert.BuildVersion
	}
	if m.src.Hub.Node != "" {
		snap.Nodes = append(snap.Nodes, hubNode(snap.Hub))
	}
	if m.src.Nodes != nil {
		snap.Nodes = append(snap.Nodes, nodes(m.src.Nodes.Statuses())...)
	}
	for i := range snap.Nodes {
		m.observedModels(&snap.Nodes[i])
	}
	if m.src.Roster != nil {
		for _, c := range m.src.Roster.All(ctx) {
			a := Agent{
				ID: c.Agent.ID, Node: m.place(c.Node), Harness: c.Harness, Snapshot: c.Snapshot,
				Model: c.Model, Models: c.Models, Eligible: c.Eligible, Why: c.Why,
				Requires: c.Agent.Requires, Level: string(c.Level.OrDefault()), Slots: c.Slots, Region: c.Region,
			}
			if !c.Eligible {
				if fix, err := m.src.Roster.Repair(ctx, c.Agent.ID); err == nil {
					a.Repair = fix.Helper.Agent.ID
				}
			}
			snap.Agents = append(snap.Agents, a)
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
	snap.Sources = []SourceHealth{
		{Name: "nodes", Wired: m.src.Nodes != nil}, {Name: "roster", Wired: m.src.Roster != nil},
		{Name: "tasks", Wired: m.src.Tasks != nil}, {Name: "plans", Wired: m.src.Plans != nil},
		{Name: "ledger", Wired: m.src.Ledger != nil}, {Name: "schedules", Wired: m.src.Schedules != nil},
	}
	snap.Schedules = []Schedule{}
	if m.src.Schedules != nil {
		for _, j := range m.src.Schedules.List("") {
			snap.Schedules = append(snap.Schedules, Schedule{ID: j.ID, Conversation: j.ConversationID, Agent: j.Member, Prompt: j.Prompt, Spec: j.Spec.Text, NextAt: j.NextAt, LastAt: j.LastAt, Runs: j.Runs})
		}
	}
	// Absence is a fact too: every list is present, empty or not, so a
	// renderer never has to guess whether "none" meant "not asked".
	if snap.Nodes == nil {
		snap.Nodes = []Node{}
	}
	if snap.Agents == nil {
		snap.Agents = []Agent{}
	}
	if snap.Tasks == nil {
		snap.Tasks = []Task{}
	}
	if snap.Plans == nil {
		snap.Plans = []Plan{}
	}
	snap.Projects = []Project{}
	if m.src.Ledger != nil {
		for _, p := range m.src.Ledger.ProjectList(ctx) {
			item := Project{
				ID: p.ID, Node: m.place(p.Home.Node), Path: p.Home.Path,
				Level: string(p.Level.OrDefault()), Repo: string(p.Repo), DefaultRole: string(p.DefaultRole), Agents: []string{},
			}
			for _, a := range snap.Agents {
				if a.Eligible && p.NotHome(nodeOf(a.Node, m.src.Hub.Node), false) == nil {
					item.Agents = append(item.Agents, a.ID)
				}
			}
			snap.Projects = append(snap.Projects, item)
		}
		sort.Slice(snap.Projects, func(i, j int) bool { return snap.Projects[i].ID < snap.Projects[j].ID })
	}
	snap.Attempts, snap.Landings = []Attempt{}, []Landing{}
	snap.Facts = Facts{Reservations: []Reservation{}, Attestations: []Attestation{}, Replicas: []Replica{}, Disclosures: []Disclosure{}, Effects: []Effect{}, Grants: []Grant{}}
	if m.src.Ledger != nil {
		if live := m.src.Ledger.LiveAttempts(ctx); live != nil {
			for i := range live {
				live[i].Node = m.place(live[i].Node)
			}
			snap.Attempts = live
		}
		if recent := m.src.Ledger.RecentLandings(ctx); recent != nil {
			snap.Landings = recent
		}
		snap.Facts = m.src.Ledger.Facts(ctx)
		closed, err := m.src.Ledger.ClosedAttempts(ctx)
		if err != nil {
			m.markSource(&snap, "ledger", err)
		}
		snap.Usage = usage(closed, snap.Tasks)
	}
	snap.Inbox = inbox(snap.Facts)
	// Activities: live attempts are the truth about "busy"; the latest
	// progress says what the attempt is doing, when it was seen at all.
	m.mu.Lock()
	observed := make(map[string]Activity, len(m.activity))
	for k, v := range m.activity {
		observed[k] = v
	}
	m.mu.Unlock()
	byAgent := map[string][]Activity{}
	for _, a := range snap.Attempts {
		act := Activity{Agent: a.Agent, AttemptID: a.ID, Kind: a.Kind, Workspace: a.Workspace, TaskID: a.TaskID, Since: a.StartedAt, At: a.StartedAt}
		if seen, ok := observed[a.Agent]; ok && seen.TaskID == a.TaskID && time.Since(seen.At) < activityFresh {
			act.StepID, act.Tool, act.Detail, act.At, act.Conversation = seen.StepID, seen.Tool, seen.Detail, seen.At, seen.Conversation
		}
		byAgent[a.Agent] = append(byAgent[a.Agent], act)
	}
	for i := range snap.Agents {
		snap.Agents[i].Activities = byAgent[snap.Agents[i].ID]
		snap.Agents[i].Busy = len(byAgent[snap.Agents[i].ID])
	}
	// Four axes per task, then each descendant's execution and attention
	// roll up into its ancestors: a top-level card answers for its tree.
	attention := map[string]int{}
	for _, r := range snap.Inbox {
		if r.TaskID != "" {
			attention[r.TaskID]++
		}
	}
	live := map[string]bool{}
	for _, a := range snap.Attempts {
		live[a.TaskID] = true
	}
	parent := map[string]string{}
	for _, t := range snap.Tasks {
		parent[t.ID] = t.Parent
	}
	rolledLive, rolledAttention := map[string]bool{}, map[string]int{}
	for _, t := range snap.Tasks {
		waiting := attention[t.ID]
		if p, ok := planByTask[t.ID]; ok {
			for _, s := range p.Steps {
				if s.State == plan.StepAwaitingHuman {
					waiting++
				}
			}
		}
		for id := t.ID; id != ""; id = parent[id] {
			if live[t.ID] {
				rolledLive[id] = true
			}
			rolledAttention[id] += waiting
		}
	}
	for i := range snap.Tasks {
		t := &snap.Tasks[i]
		t.Lifecycle = t.State
		t.Execution = "idle"
		if rolledLive[t.ID] {
			t.Execution = "running"
		}
		t.Attention = rolledAttention[t.ID]
		t.Lane = lane(*t)
	}
	return snap
}

// markSource records that a source failed to contribute.
func (m *Model) markSource(snap *Snapshot, name string, err error) {
	for i := range snap.Sources {
		if snap.Sources[i].Name == name {
			snap.Sources[i].Error = err.Error()
		}
	}
}

// lane is the board column the four axes imply. It is total over the
// task state machine: every lifecycle state lands somewhere, and nothing
// is called "queued" — there is no queue on record to count.
func lane(t Task) string {
	switch {
	case t.Attention > 0:
		return "needs_you"
	case t.Execution == "running":
		return "running"
	}
	switch t.Lifecycle {
	case "done", "cancelled":
		return "ended"
	case "paused":
		return "set_aside"
	case "failed", "blocked", "review":
		// Recoverable, and only a person decides how: continue or cancel.
		return "needs_you"
	default:
		// Open and idle: waiting for its next line, or for its turn.
		return "pending"
	}
}

// inbox projects what only a person can settle, with the commands that
// settle it. The operations behind these stay the authority.
func inbox(f Facts) []HumanRequest {
	out := []HumanRequest{}
	for _, d := range f.Disclosures {
		out = append(out, HumanRequest{
			ID: d.ID, Type: "disclosure", Source: d.ID, ProjectID: d.Project, TaskID: d.TaskID, CreatedAt: d.At, Resolvable: true,
			Summary: fmt.Sprintf("task #%s wants to send %d bytes out of sealed project %s (asked by %s)", d.TaskID, d.Bytes, d.Project, d.Requester),
			Choices: []Choice{{Label: "approve", Command: "/approve " + d.ID}, {Label: "deny", Command: "/deny " + d.ID, Danger: true}},
		})
	}
	for _, e := range f.Effects {
		out = append(out, HumanRequest{
			ID: e.ID, Type: "effect", Source: e.Attempt, TaskID: e.TaskID, CreatedAt: e.At, Resolvable: true,
			Summary: fmt.Sprintf("%s was called for task #%s and nobody knows whether it happened%s", e.Tool, e.TaskID, errSuffix(e.Error)),
			Choices: []Choice{{Label: "confirm it happened", Command: "/effects " + e.ID + " happened"}, {Label: "allow it to be called again", Command: "/effects " + e.ID + " new", Danger: true}},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func errSuffix(err string) string {
	if err == "" {
		return ""
	}
	return ": " + err
}

// observedModels fills a node's harness model lists from what was seen
// running there, for harnesses whose config declares none. The node's
// name in the book is the model's: "" for the hub.
func (m *Model) observedModels(n *Node) {
	if m.src.Models == nil {
		return
	}
	node := n.Name
	if n.Role == RoleHub {
		node = ""
	}
	for i := range n.Harnesses {
		h := &n.Harnesses[i]
		if seen, ok := m.src.Models.Get(node, h.ID); ok {
			if len(h.Models) == 0 {
				h.Models = seen.Available
			}
			h.Model = seen.Current
			h.Version = seen.Version
		}
	}
}

// recentActivity is an agent's activity if it is fresh enough to mean
// "now": a progress stream that went quiet two minutes ago is history.
func (m *Model) recentActivity(agent string) (Activity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	act, ok := m.activity[agent]
	if !ok || time.Since(act.At) > activityFresh {
		return Activity{}, false
	}
	return act, true
}

const activityFresh = 2 * time.Minute

// usage folds every closed attempt into the totals the Usage view shows.
// The ledger's attempt records are the authority; the task list only
// supplies the day-of-start for attempts whose record lacks one.
func usage(closed []attempt.Record, tasks []Task) Usage {
	byDay, byAgent, byModel := map[string]*UsageRow{}, map[string]*UsageRow{}, map[string]*UsageRow{}
	total := UsageRow{Key: "total"}
	add := func(day, agent, model string, tokens Tokens, seconds int64) {
		for key, table := range map[string]map[string]*UsageRow{day: byDay, agent: byAgent, model: byModel} {
			if key == "" {
				continue
			}
			row := table[key]
			if row == nil {
				row = &UsageRow{Key: key}
				table[key] = row
			}
			row.Tokens = row.Tokens.add(tokens)
			row.Seconds += seconds
			row.Attempts++
		}
		total.Tokens = total.Tokens.add(tokens)
		total.Seconds += seconds
		total.Attempts++
	}
	_ = tasks
	for _, r := range closed {
		var tokens Tokens
		model := ""
		if u := r.Usage; u != nil {
			tokens = Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output, Context: u.Context}
			model = u.Model
			if !u.Reported {
				total.Unreported++
			}
		} else {
			total.Unreported++
		}
		var seconds int64
		if !r.EndedAt.IsZero() && !r.StartedAt.IsZero() {
			seconds = int64(r.EndedAt.Sub(r.StartedAt).Seconds())
		}
		add(r.StartedAt.Local().Format("2006-01-02"), r.Agent, model, tokens, seconds)
	}
	flatten := func(table map[string]*UsageRow) []UsageRow {
		out := make([]UsageRow, 0, len(table))
		for _, row := range table {
			out = append(out, *row)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out
	}
	return Usage{ByDay: flatten(byDay), ByAgent: flatten(byAgent), ByModel: flatten(byModel), Total: total}
}

// nodeOf is place's inverse: the hub's name back to the model's "".
func nodeOf(placed, hub string) string {
	if placed == hub {
		return ""
	}
	return placed
}

// place resolves the model's "" — this machine — to the hub's node name,
// so nothing downstream renders a role where a machine belongs.
func (m *Model) place(node string) string {
	if node == "" {
		return m.src.Hub.Node
	}
	return node
}

// hubNode is the hub's own machine as a node: role hub, always up (it is
// answering), described by its own advert.
func hubNode(h Hub) Node {
	caps := h.Capabilities
	if caps == nil {
		caps = h.Advert.Capabilities
	}
	n := Node{
		Name: h.Node, Role: RoleHub, Version: h.Advert.BuildVersion, Up: true, Since: h.Started, Snapshot: nodewire.Synthesize(h.Advert, time.Now()), Features: h.Advert.Features,
		Host: h.Advert.Hostname, IPs: h.Advert.IPs, OS: h.Advert.OS, Arch: h.Advert.Arch,
		Capabilities: caps, Level: h.Level, Harnesses: harnesses(h.Advert),
	}
	if n.Level == "" {
		n.Level = "internal"
	}
	return n
}

func harnesses(adv nodewire.Advert) []Harness {
	out := make([]Harness, 0, len(adv.Harnesses))
	for _, h := range adv.Harnesses {
		out = append(out, Harness{ID: h.ID, Slots: h.Slots, Models: h.Models, Missing: h.Missing})
	}
	return out
}

func nodes(statuses []node.Status) []Node {
	out := make([]Node, 0, len(statuses))
	for _, s := range statuses {
		n := Node{
			Name: s.Name, Role: RoleWorker, Version: s.Advert.BuildVersion, Addr: s.Addr, Up: s.Up, Since: s.Since, Snapshot: nodewire.Synthesize(s.Advert, time.Now()), Features: s.Advert.Features,
			Host: s.Advert.Hostname, IPs: s.Advert.IPs, OS: s.Advert.OS, Arch: s.Advert.Arch,
			Capabilities: s.Advert.Capabilities, LastError: s.LastError, Level: s.Level, Region: s.Region,
			Harnesses: harnesses(s.Advert),
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
			Channel: t.Channel, ProjectID: t.ProjectID, Origin: t.Origin, Requester: t.Requester,
			Turns: t.Budget.Turns, MaxTurns: t.Budget.MaxTurns,
			Elapsed:   t.Budget.Elapsed.Round(time.Second).String(),
			MaxElapse: t.Budget.MaxElapsed.Round(time.Minute).String(),
			UpdatedAt: t.UpdatedAt,
		}
		if p, ok := plans[t.ID]; ok {
			item.PlanID = p.ID
		}
		for _, a := range t.Attempts {
			row := AttemptRow{
				Day: a.StartedAt.UTC().Format("2006-01-02"), Agent: a.Member, Node: a.Node, Model: a.Model, Outcome: string(a.Outcome), Started: a.StartedAt,
				Tokens:   Tokens{Input: a.Tokens.Input, Output: a.Tokens.Output, CachedRead: a.Tokens.CachedRead, CachedWrite: a.Tokens.CachedWrite, Total: a.Tokens.Total},
				Reported: a.Tokens.Input+a.Tokens.Output+a.Tokens.CachedRead > 0,
			}
			if !a.EndedAt.IsZero() {
				row.Seconds = int64(a.EndedAt.Sub(a.StartedAt).Seconds())
			}
			item.AttemptRows = append(item.AttemptRows, row)
			item.Tokens = item.Tokens.add(row.Tokens)
			item.Seconds += row.Seconds
			if a.Model != "" {
				item.Model = a.Model
			}
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
			step.StartedAt, step.EndedAt = s.Result.StartedAt, s.Result.EndedAt
			if u := s.Result.Usage; u != nil {
				usage := StepUsage{Day: s.Result.StartedAt.UTC().Format("2006-01-02"), Model: u.Model, Tokens: Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}}
				if !s.Result.EndedAt.IsZero() && !s.Result.StartedAt.IsZero() {
					usage.Seconds = int64(s.Result.EndedAt.Sub(s.Result.StartedAt).Seconds())
				}
				step.Usage = &usage
			}
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

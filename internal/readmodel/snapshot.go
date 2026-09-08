package readmodel

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// Snapshot reads every source once. It never fails: a source that is not
// wired simply contributes nothing, because a dashboard that goes blank when
// one subsystem is off is worse than one that shows the rest.
//
// The sections run in the order the page reads them, and later ones
// depend on earlier ones: projects ask which workspaces the live attempts
// keep busy, activities pair the attempts with the agents, and the task
// axes at the end roll the inbox, the attempts and the plans up the tree.
func (m *Model) Snapshot(ctx context.Context) Snapshot {
	b := &snapshotBuilder{m: m, snap: Snapshot{At: time.Now(), Hub: m.src.Hub}}
	b.fleet()
	b.agents(ctx)
	b.plansAndTasks()
	b.sources()
	b.schedules()
	b.liveAttempts(ctx)
	b.projects(ctx)
	b.ledgerFacts(ctx)
	b.inbox()
	b.activities()
	b.taskAxes()
	return b.snap
}

// snapshotBuilder is one Snapshot in progress: the snapshot itself and
// the facts its sections hand each other.
type snapshotBuilder struct {
	m    *Model
	snap Snapshot
	// planByTask indexes the plans by the task they belong to.
	planByTask map[string]plan.Plan
	// live are the ledger's attempts in flight, placed on nodes.
	live []Attempt
	// activityKnown says the live attempts were read completely; every
	// ActivityKnown pointer in the snapshot points here. attentionKnown
	// says the same for everything the inbox is made of.
	activityKnown  bool
	attentionKnown bool
	// closed are the finished attempts usage is computed from.
	closed []attempt.Record
}

// fleet lists the hub's machine first, then the workers, and fills each
// node's harnesses with the models seen running there.
func (b *snapshotBuilder) fleet() {
	m, snap := b.m, &b.snap
	if m.src.HubAdvert != nil {
		snap.Hub.Advert = m.src.HubAdvert()
		snap.Hub.Version = snap.Hub.Advert.BuildVersion
	}
	if m.src.Hub.Node != "" {
		snap.Nodes = append(snap.Nodes, hubNode(snap.Hub))
	}
	if m.src.Nodes != nil {
		for _, worker := range nodes(m.src.Nodes.Statuses()) {
			if len(snap.Nodes) > 0 && worker.Name == snap.Hub.Node {
				worker.Role = RoleHub
				snap.Nodes[0] = worker
			} else {
				snap.Nodes = append(snap.Nodes, worker)
			}
		}
	}
	for i := range snap.Nodes {
		m.observedModels(&snap.Nodes[i])
	}
	// Absence is a fact too: every list is present, empty or not, so a
	// renderer never has to guess whether "none" meant "not asked".
	if snap.Nodes == nil {
		snap.Nodes = []Node{}
	}
}

// agents is the roster: each agent with its placement, its admission
// verdict per requirement, and who could repair it when blocked.
func (b *snapshotBuilder) agents(ctx context.Context) {
	m, snap := b.m, &b.snap
	if m.src.Roster != nil {
		for _, c := range m.src.Roster.All(ctx) {
			a := Agent{
				ID: c.Agent.ID, Node: m.place(c.Node), Harness: c.Harness, Snapshot: c.Snapshot,
				Model: c.Model, Models: c.Models, Eligible: c.Eligible, Why: c.Why,
				Requires: c.Agent.Requires, Level: string(c.Level.OrDefault()), Slots: c.Slots, Region: c.Region,
				Preferred: c.Agent.Model, Observed: c.Observed, MCPServers: c.Agent.MCPServers, Default: c.Agent.Default,
				Options: c.Agent.Options, Selectors: c.Selectors, About: c.Agent.About,
			}
			if req, err := ability.Compile(c.Agent.Requires); err == nil && !req.Empty() {
				for _, atom := range c.Match(req).Atoms {
					a.Conditions = append(a.Conditions, Condition{Atom: atom.Atom, Met: atom.Verdict == ability.True, Code: string(atom.Code), Detail: atom.Detail})
				}
			}
			if !c.Eligible {
				if fix, err := m.src.Roster.Repair(ctx, c.Agent.ID); err == nil {
					a.Repair = fix.Helper.Agent.ID
				}
			}
			snap.Agents = append(snap.Agents, a)
		}
	}
	if snap.Agents == nil {
		snap.Agents = []Agent{}
	}
}

// plansAndTasks lists the plans, then the tasks with their plan and
// their metadata.
func (b *snapshotBuilder) plansAndTasks() {
	m, snap := b.m, &b.snap
	b.planByTask = map[string]plan.Plan{}
	if m.src.Plans != nil {
		for _, p := range m.src.Plans.List() {
			snap.Plans = append(snap.Plans, convertPlan(p))
			if p.TaskID != "" {
				b.planByTask[p.TaskID] = p
			}
		}
	}
	if m.src.Tasks != nil {
		snap.Tasks = tasks(m.src.Tasks.List(""), b.planByTask)
		for i := range snap.Tasks {
			t := &snap.Tasks[i]
			meta := m.src.Tasks.MetaOf(t.ID)
			t.Title, t.Priority, t.Labels = meta.Title, meta.Priority, meta.Labels
			if meta.ArchivedAt != nil {
				t.ArchivedAt = meta.ArchivedAt.Format(time.RFC3339)
			}
		}
	}
	if snap.Tasks == nil {
		snap.Tasks = []Task{}
	}
	if snap.Plans == nil {
		snap.Plans = []Plan{}
	}
}

// sources declares which sources are wired; the ledger sections below
// mark the ones that then failed to answer.
func (b *snapshotBuilder) sources() {
	m, snap := b.m, &b.snap
	snap.Sources = []SourceHealth{
		{Name: "nodes", Wired: m.src.Nodes != nil}, {Name: "roster", Wired: m.src.Roster != nil},
		{Name: "tasks", Wired: m.src.Tasks != nil}, {Name: "plans", Wired: m.src.Plans != nil},
		{Name: "ledger", Wired: m.src.Ledger != nil}, {Name: "schedules", Wired: m.src.Schedules != nil},
	}
	for _, name := range []string{"ledger-live", "ledger-projects", "ledger-landings", "ledger-facts", "ledger-attention", "ledger-usage"} {
		snap.Sources = append(snap.Sources, SourceHealth{Name: name, Wired: m.src.Ledger != nil})
	}
}

func (b *snapshotBuilder) schedules() {
	m, snap := b.m, &b.snap
	snap.Schedules = []Schedule{}
	if m.src.Schedules != nil {
		for _, j := range m.src.Schedules.List("") {
			snap.Schedules = append(snap.Schedules, Schedule{ID: j.ID, Conversation: j.ConversationID, Agent: j.Member, Prompt: j.Prompt, Spec: j.Spec.Text, NextAt: j.NextAt, LastAt: j.LastAt, Runs: j.Runs, State: j.State, Error: j.Error, PendingKey: j.PendingKey})
		}
	}
}

// liveAttempts reads the attempts in flight. A failed read is marked on
// the live and attention sources, and whatever came back is kept: a
// partial list is still evidence of what is busy.
func (b *snapshotBuilder) liveAttempts(ctx context.Context) {
	m, snap := b.m, &b.snap
	if m.src.Ledger == nil {
		return
	}
	var err error
	b.live, err = m.src.Ledger.LiveAttempts(ctx)
	b.activityKnown = err == nil
	m.markLedgerSource(snap, "live", err)
	if err != nil {
		m.markSource(snap, "ledger-attention", fmt.Errorf("writers: %w", err))
	}
	for i := range b.live {
		b.live[i].Node = m.place(b.live[i].Node)
	}
}

// projects lists each project with its workspaces: whether a live attempt
// keeps one busy, what repositories it holds, and which eligible agents
// the project would place there.
func (b *snapshotBuilder) projects(ctx context.Context) {
	m, snap := b.m, &b.snap
	snap.Projects = []Project{}
	if m.src.Ledger == nil {
		return
	}
	projects, err := m.src.Ledger.ProjectList(ctx)
	m.markLedgerSource(snap, "projects", err)
	for _, p := range projects {
		item := Project{
			ID: p.ID, Node: m.place(p.Home.Node), Path: p.Home.Path,
			Level: string(p.Level.OrDefault()), Repo: string(p.Repo), DefaultRole: string(p.DefaultRole), Agents: []string{},
			Home: p.ID == m.src.HomeProject, Default: p.ID == m.src.DefaultProject,
		}
		item.Workspaces = []Workspace{}
		for _, ws := range p.Workspaces() {
			w := b.workspace(p, ws)
			for _, id := range w.Agents {
				if !slices.Contains(item.Agents, id) {
					item.Agents = append(item.Agents, id)
				}
			}
			if ws.Kind == project.KindCanonical {
				item.Repos = w.Repos
			}
			item.Workspaces = append(item.Workspaces, w)
		}
		snap.Projects = append(snap.Projects, item)
	}
	sort.Slice(snap.Projects, func(i, j int) bool { return snap.Projects[i].ID < snap.Projects[j].ID })
}

// workspace describes one of a project's workspaces: its copy state, the
// live attempt keeping it busy, its repositories and the eligible agents
// the project places there.
func (b *snapshotBuilder) workspace(p project.Project, ws project.Workspace) Workspace {
	m := b.m
	w := Workspace{ID: ws.ID, Node: m.place(ws.Node), Path: ws.Path, Kind: string(ws.Kind), Agents: []string{}, ActivityKnown: &b.activityKnown}
	if c, ok := p.CopyOn(ws.Node); ok {
		w.Origin, w.Source, w.State, w.Error = string(c.Origin), c.Source, string(c.State), c.Error
	}
	for _, a := range b.live {
		if a.Node == w.Node && a.Workspace == ws.Path {
			w.Busy = true
		}
	}
	if m.src.Repos != nil {
		w.Repos = m.src.Repos(ws.ID)
	}
	for _, a := range b.snap.Agents {
		if a.Eligible && ws.Kind == project.KindCanonical && nodeOf(a.Node, m.src.Hub.Node) == ws.Node || a.Eligible && ws.Kind != project.KindCanonical && placed(p, nodeOf(a.Node, m.src.Hub.Node), ws.ID) {
			w.Agents = append(w.Agents, a.ID)
		}
	}
	return w
}

// ledgerFacts reads the rest of the ledger: the attempts in flight become
// the snapshot's, then the recent landings, the facts a person may want
// at a glance, and the closed attempts usage is computed from. Attention
// is known only when both the facts and the live attempts were read
// completely.
func (b *snapshotBuilder) ledgerFacts(ctx context.Context) {
	m, snap := b.m, &b.snap
	snap.Attempts, snap.Landings = []Attempt{}, []Landing{}
	snap.Facts = Facts{Reservations: []Reservation{}, Attestations: []Attestation{}, Replicas: []Replica{}, Disclosures: []Disclosure{}, Effects: []Effect{}, Grants: []Grant{}}
	if m.src.Ledger == nil {
		return
	}
	if b.live != nil {
		snap.Attempts = b.live
	}
	recent, err := m.src.Ledger.RecentLandings(ctx)
	m.markLedgerSource(snap, "landings", err)
	if recent != nil {
		snap.Landings = recent
	}
	snap.Facts, err = m.src.Ledger.Facts(ctx)
	factsAttentionKnown := err == nil || snap.Facts.attentionKnown
	b.attentionKnown = factsAttentionKnown && b.activityKnown
	m.markLedgerSource(snap, "facts", err)
	if !factsAttentionKnown {
		m.markSource(snap, "ledger-attention", err)
	}
	b.closed, err = m.src.Ledger.ClosedAttempts(ctx)
	m.markLedgerSource(snap, "usage", err)
}

// inbox is what only a person can settle: the ledger's disclosures,
// effects and unsettled writers, then the pending questions.
func (b *snapshotBuilder) inbox() {
	snap := &b.snap
	normalizeFacts(&snap.Facts)
	snap.Usage = usage(b.closed, snap.At, snap.Tasks)
	snap.Inbox = inbox(snap.Facts, snap.Attempts)
	snap.Inbox = append(snap.Inbox, b.m.pendingInteractions()...)
}

// activities pairs each agent with its attempts in flight: live attempts
// are the truth about "busy"; the latest progress says what the attempt
// is doing, when it was seen at all.
func (b *snapshotBuilder) activities() {
	m, snap := b.m, &b.snap
	m.mu.Lock()
	observed := make(map[string]Activity, len(m.activity))
	for k, v := range m.activity {
		observed[k] = v
	}
	m.mu.Unlock()
	byAgent := map[string][]Activity{}
	for _, a := range snap.Attempts {
		act := Activity{Agent: a.Agent, AttemptID: a.ID, Kind: a.Kind, Workspace: a.Workspace, TaskID: a.TaskID, Since: a.StartedAt, At: a.StartedAt}
		if a.Unsettled {
			act.Kind = "writer"
			act.Detail = "原执行进程是否退出尚未确认" + errSuffix(a.Error)
		} else if seen, ok := observed[a.Agent]; ok && seen.TaskID == a.TaskID && time.Since(seen.At) < activityFresh {
			act.StepID, act.Tool, act.Detail, act.At, act.Conversation = seen.StepID, seen.Tool, seen.Detail, seen.At, seen.Conversation
		}
		byAgent[a.Agent] = append(byAgent[a.Agent], act)
	}
	for i := range snap.Agents {
		snap.Agents[i].Activities = byAgent[snap.Agents[i].ID]
		snap.Agents[i].Busy = len(byAgent[snap.Agents[i].ID])
		snap.Agents[i].ActivityKnown = &b.activityKnown
	}
}

// taskAxes sets the four axes per task, then rolls each descendant's
// execution and attention up into its ancestors: a top-level card answers
// for its tree.
func (b *snapshotBuilder) taskAxes() {
	snap := &b.snap
	attention := map[string]int{}
	for _, r := range snap.Inbox {
		if r.TaskID != "" {
			attention[r.TaskID]++
		}
	}
	live, unsettled := map[string]bool{}, map[string]bool{}
	for _, a := range snap.Attempts {
		if a.Unsettled {
			unsettled[a.TaskID] = true
		} else {
			live[a.TaskID] = true
		}
	}
	parent := map[string]string{}
	for _, t := range snap.Tasks {
		parent[t.ID] = t.Parent
	}
	rolledLive, rolledUnsettled, rolledAttention := map[string]bool{}, map[string]bool{}, map[string]int{}
	for _, t := range snap.Tasks {
		waiting := attention[t.ID]
		if p, ok := b.planByTask[t.ID]; ok {
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
			if unsettled[t.ID] {
				rolledUnsettled[id] = true
			}
			rolledAttention[id] += waiting
		}
	}
	for i := range snap.Tasks {
		t := &snap.Tasks[i]
		t.Lifecycle = t.State
		t.Execution = ExecutionIdle
		if !b.activityKnown || rolledUnsettled[t.ID] {
			t.Execution = ExecutionUnknown
		}
		if rolledLive[t.ID] {
			t.Execution = ExecutionRunning
		}
		t.Attention = rolledAttention[t.ID]
		t.Lane = lane(*t)
		if t.Lane == "pending" && !b.attentionKnown {
			t.Lane = "unknown"
		}
	}
}

func normalizeFacts(f *Facts) {
	if f.Reservations == nil {
		f.Reservations = []Reservation{}
	}
	if f.Attestations == nil {
		f.Attestations = []Attestation{}
	}
	if f.Replicas == nil {
		f.Replicas = []Replica{}
	}
	if f.Disclosures == nil {
		f.Disclosures = []Disclosure{}
	}
	if f.Effects == nil {
		f.Effects = []Effect{}
	}
	if f.Grants == nil {
		f.Grants = []Grant{}
	}
}

func (m *Model) markLedgerSource(snap *Snapshot, query string, err error) {
	if err == nil {
		return
	}
	m.markSource(snap, "ledger-"+query, err)
	m.markSource(snap, "ledger", fmt.Errorf("%s: %w", query, err))
}

// markSource records that a source failed to contribute.
func (m *Model) markSource(snap *Snapshot, name string, err error) {
	for i := range snap.Sources {
		if snap.Sources[i].Name == name {
			if snap.Sources[i].Error != "" {
				snap.Sources[i].Error += "; "
			}
			snap.Sources[i].Error += err.Error()
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
	case t.Execution == ExecutionRunning:
		return "running"
	}
	switch t.Lifecycle {
	case task.StateDone, task.StateCancelled:
		return "ended"
	case task.StatePaused:
		return "set_aside"
	case task.StateFailed, task.StateBlocked, task.StateReview:
		// Recoverable, and only a person decides how: continue or cancel.
		return "needs_you"
	default:
		if t.Execution == ExecutionUnknown {
			return "unknown"
		}
		// Open and idle: waiting for its next line, or for its turn.
		return "pending"
	}
}

// inbox projects what only a person can settle, with the commands that
// settle it. The operations behind these stay the authority.
func inbox(f Facts, attempts []Attempt) []HumanRequest {
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
	for _, a := range attempts {
		if !a.Unsettled {
			continue
		}
		out = append(out, HumanRequest{
			ID: "writer:" + a.ID, Type: "writer", Source: a.ID, AttemptID: a.ID, Node: a.Node, Workspace: a.Workspace,
			ProjectID: a.Project, TaskID: a.TaskID, CreatedAt: a.StartedAt,
			Summary: "原执行进程是否退出尚未确认，目录与执行资源继续保留占用" + errSuffix(a.Error),
			Choices: []Choice{}, Resolvable: false,
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
		Name: h.Node, Role: RoleHub, Version: h.Advert.BuildVersion, Up: true, Since: h.Started, Snapshot: nodewire.Synthesize(h.Advert, time.Now()), Features: h.Advert.Features, Health: h.Advert.Health,
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
			Name: s.Name, Role: RoleWorker, Version: s.Advert.BuildVersion, Addr: s.Addr, Up: s.Up, Since: s.Since, Snapshot: nodewire.Synthesize(s.Advert, time.Now()), Features: s.Advert.Features, Health: s.Advert.Health,
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
			ID: t.ID, Goal: t.Goal, State: t.State, Member: t.Member,
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

// placed says whether an agent on node would work in the workspace: the
// project's one rule, asked once here for the page.
func placed(p project.Project, node, workspaceID string) bool {
	ws, err := p.Place(node)
	return err == nil && ws.ID == workspaceID
}

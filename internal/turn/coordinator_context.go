package turn

import (
	"context"
	"github.com/gopact-ai/steve/internal/home"
	"regexp"
	"sort"
	"strings"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// Context is where a conversation stands: which project it works in, on
// which machine, with which agent, and which agents could take its next
// line. It is what the page's context bar shows, computed by the same
// rules a turn is judged by, so the bar never promises what a turn would
// refuse.
type Context struct {
	Conversation string
	Project      *ContextProject
	Agent        *AgentChoice
	Agents       []AgentChoice
}

type ContextProject struct {
	ID      string
	Node    string
	Path    string
	Level   string
	Repo    string
	Version int64
	// Bound is false when the conversation has no binding yet and the
	// project shown is the default it would get on its first line.
	Bound bool
}

// projectFor is the conversation's project without side effects: its
// binding if it has one, else the default it would be bound to.
func (c *Coordinator) projectFor(ctx context.Context, conversationID string) (id string, version int64, bound bool, err error) {
	binding, ok, err := c.projects.Binding(ctx, conversationID)
	if err != nil {
		return "", 0, false, err
	}
	if ok {
		return binding.ProjectID, binding.Version, true, nil
	}
	id = c.defaultProject
	if c.homeProject != "" && injectionMode(protocol.ChatP2P, c.ownerOpenID, c.ownerOpenID) == home.ModeOwner {
		id = c.homeProject
	}
	return id, 0, false, nil
}

// Suggestion is one completion for a line being typed.
type Suggestion struct {
	Label  string
	Args   string
	Detail string
	Insert string
	Muted  bool
}

// Suggest completes the line so far, by the rules the line will be judged
// by: verbs from the catalogue, agents with their admission here, projects,
// and only this conversation's tasks — the page keeps no rules of its own.
func (c *Coordinator) Suggest(ctx context.Context, conversationID, line string) []Suggestion {
	line = strings.TrimLeft(line, " ")
	if line == "" || strings.Contains(line, "\n") {
		return nil
	}
	if at := atMention.FindStringSubmatch(line); at != nil {
		head := line[:len(line)-len(at[2])-1]
		context, err := c.Context(ctx, conversationID)
		if err != nil {
			return nil
		}
		var out []Suggestion
		for _, a := range context.Agents {
			if !strings.HasPrefix(a.ID, at[2]) {
				continue
			}
			detail := a.Node + " · " + a.Harness
			if a.Model != "" {
				detail += " · " + a.Model
			}
			if a.Usable {
				detail += " · " + c.text.T(i18n.SuggestUsable)
			} else {
				detail += " · " + a.Because
			}
			out = append(out, Suggestion{Label: "@" + a.ID, Detail: detail, Insert: head + "@" + a.ID + " ", Muted: !a.Usable})
		}
		return out
	}
	if !strings.HasPrefix(line, "/") {
		return nil
	}
	verb, rest, hasSpace := strings.Cut(line, " ")
	if !hasSpace {
		var out []Suggestion
		for _, v := range c.Verbs() {
			if strings.HasPrefix(v.Command, verb) {
				insert := v.Command
				if v.Args != "" {
					insert += " "
				}
				out = append(out, Suggestion{Label: v.Command, Args: v.Args, Detail: v.Summary, Insert: insert})
			}
		}
		return out
	}
	rest = strings.TrimLeft(rest, " ")
	switch protocol.Command(verb) {
	case protocol.CommandProject:
		if !strings.HasPrefix(rest, "use") {
			return []Suggestion{{Label: "/project use", Args: "<id>", Detail: c.text.T(i18n.VerbProject), Insert: "/project use "}}
		}
		want := strings.TrimSpace(strings.TrimPrefix(rest, "use"))
		if c.projects == nil {
			return nil
		}
		all, err := c.projects.List(ctx)
		if err != nil {
			return nil
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		var out []Suggestion
		for _, p := range all {
			if !strings.HasPrefix(p.ID, want) {
				continue
			}
			out = append(out, Suggestion{Label: p.ID, Detail: nodewire.Place(p.Home.Node) + " · " + p.Home.Path + " · " + string(p.Level.OrDefault()) + " · " + string(p.Repo), Insert: "/project use " + p.ID})
		}
		return out
	case protocol.CommandUse, protocol.CommandRepair:
		context, err := c.Context(ctx, conversationID)
		if err != nil {
			return nil
		}
		var out []Suggestion
		for _, a := range context.Agents {
			if !strings.HasPrefix(a.ID, rest) {
				continue
			}
			if protocol.Command(verb) == protocol.CommandRepair {
				if a.Ready {
					continue
				}
				if _, err := c.fleet.Repair(ctx, a.ID); err != nil {
					continue
				}
			}
			detail := a.Node + " · " + a.Harness
			if a.Because != "" {
				detail += " · " + a.Because
			}
			out = append(out, Suggestion{Label: a.ID, Detail: detail, Insert: verb + " " + a.ID, Muted: protocol.Command(verb) == protocol.CommandUse && !a.Usable})
		}
		return out
	case protocol.CommandTasks:
		if c.tasks == nil {
			return nil
		}
		op, want := "", rest
		for _, o := range []string{"pause", "resume", "cancel"} {
			if strings.HasPrefix(rest, o+" ") {
				op, want = o, strings.TrimSpace(strings.TrimPrefix(rest, o))
			}
		}
		var out []Suggestion
		if op == "" {
			for _, o := range []string{"pause", "resume", "cancel"} {
				if strings.HasPrefix(o, rest) {
					out = append(out, Suggestion{Label: o, Args: "<id>", Insert: "/tasks " + o + " "})
				}
			}
		}
		// Only this conversation's tasks: the verb refuses any other.
		list := c.tasks.List(conversationID)
		for i := len(list) - 1; i >= 0 && len(out) < 15; i-- {
			t := list[i]
			if !strings.HasPrefix(t.ID, want) {
				continue
			}
			insert := "/tasks " + t.ID
			if op != "" {
				insert = "/tasks " + op + " " + t.ID
			}
			out = append(out, Suggestion{Label: "#" + t.ID, Detail: string(t.State) + " · " + t.Member + " · " + clip(t.Goal, 80), Insert: insert})
		}
		return out
	case protocol.CommandApprove, protocol.CommandDeny:
		if c.projects == nil {
			return nil
		}
		pending, err := c.projects.PendingDisclosures(ctx)
		if err != nil {
			return nil
		}
		var out []Suggestion
		for _, d := range pending {
			if strings.HasPrefix(d.ID, rest) {
				out = append(out, Suggestion{Label: d.ID, Detail: d.Project + " · " + d.Requester, Insert: verb + " " + d.ID})
			}
		}
		return out
	case protocol.CommandEffects:
		if c.intents == nil {
			return nil
		}
		unresolved, err := c.intents.Unresolved(ctx)
		if err != nil {
			return nil
		}
		var out []Suggestion
		for _, e := range unresolved {
			if strings.HasPrefix(e.ID, rest) {
				out = append(out, Suggestion{Label: e.ID, Detail: e.Tool + " · task #" + e.TaskID, Insert: "/effects " + e.ID + " "})
			}
		}
		return out
	}
	return nil
}

var atMention = regexp.MustCompile(`(^|\s)@([\w-]*)$`)

// AgentChoice is one agent as a candidate for this conversation. Ready is
// the roster's word (the machine is up, the runtime is there); Usable
// adds the project's: the agent is where the project's workspace is.
type AgentChoice struct {
	ID      string
	Node    string
	Harness string
	Model   string
	Ready   bool
	Why     string
	Usable  bool
	Because string
	Current bool
}

// Context answers for one conversation.
func (c *Coordinator) Context(ctx context.Context, conversationID string) (Context, error) {
	out := Context{Conversation: conversationID}
	var current *project.Project
	if c.projects != nil {
		// Read-only: a query must not bind the conversation as a turn would.
		id, version, bound, err := c.projectFor(ctx, conversationID)
		if err != nil {
			return out, err
		}
		if p, ok, err := c.projects.Get(ctx, id); err == nil && ok {
			current = &p
			out.Project = &ContextProject{
				ID: p.ID, Node: nodewire.Place(p.Home.Node), Path: p.Home.Path,
				Level: string(p.Level.OrDefault()), Repo: string(p.Repo), Version: version, Bound: bound,
			}
		}
	}
	active := ""
	if c.store != nil {
		active = c.store.Conversation(conversationID).ActiveAgent
	}
	if active == "" && c.catalog != nil {
		active = c.catalog.Default().ID
	}
	if c.fleet != nil {
		for _, cand := range c.fleet.All(ctx) {
			choice := AgentChoice{
				ID: cand.Agent.ID, Node: nodewire.Place(cand.Node), Harness: cand.Harness, Model: cand.Model,
				Ready: cand.Eligible, Why: cand.Why, Usable: cand.Eligible, Current: cand.Agent.ID == active,
			}
			if !cand.Eligible {
				choice.Because = cand.Why
			} else if current != nil {
				if err := current.NotHome(cand.Node, false); err != nil {
					choice.Usable = false
					choice.Because = c.text.T(i18n.ContextNotHome, current.ID, nodewire.Place(current.Home.Node), choice.Node)
				}
			}
			out.Agents = append(out.Agents, choice)
		}
	} else if c.catalog != nil {
		for _, a := range c.catalog.List() {
			out.Agents = append(out.Agents, AgentChoice{ID: a.ID, Node: nodewire.Place(a.Node), Harness: a.Harness, Model: a.Model, Ready: true, Usable: true, Current: a.ID == active})
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		a, b := out.Agents[i], out.Agents[j]
		if a.Usable != b.Usable {
			return a.Usable
		}
		return a.ID < b.ID
	})
	for i := range out.Agents {
		if out.Agents[i].Current {
			choice := out.Agents[i]
			out.Agent = &choice
		}
	}
	return out, nil
}

// Verb is one thing the console can be told, with a line saying what it
// does, so the page can offer the verbs rather than assume they are known.
type Verb struct {
	Command string
	Args    string
	Summary string
}

// Verbs lists the console's verbs, in the order a person would look.
func (c *Coordinator) Verbs() []Verb {
	t := c.text.T
	return []Verb{
		{string(protocol.CommandPlan), "目标", t(i18n.VerbPlan)},
		{string(protocol.CommandProject), "use <id>", t(i18n.VerbProject)},
		{string(protocol.CommandTasks), "[id | pause id | resume id | cancel id]", t(i18n.VerbTasks)},
		{string(protocol.CommandPlans), "[id]", t(i18n.VerbPlans)},
		{string(protocol.CommandFleet), "[probe]", t(i18n.VerbFleet)},
		{string(protocol.CommandRepair), "<agent>", t(i18n.VerbRepair)},
		{string(protocol.CommandUse), "<agent>", t(i18n.VerbUse)},
		{string(protocol.CommandModel), "[name]", t(i18n.VerbModel)},
		{string(protocol.CommandStatus), "", t(i18n.VerbStatus)},
		{string(protocol.CommandNew), "", t(i18n.VerbNew)},
		{string(protocol.CommandClear), "", t(i18n.VerbClear)},
		{string(protocol.CommandCancel), "", t(i18n.VerbCancel)},
		{string(protocol.CommandHistory), "", t(i18n.VerbHistory)},
		{string(protocol.CommandSkills), "[list | add <ref> | update]", t(i18n.VerbSkills)},
		{string(protocol.CommandEvery), "<when> <what>", t(i18n.VerbEvery)},
		{string(protocol.CommandAt), "<when> <what>", t(i18n.VerbAt)},
		{string(protocol.CommandSchedules), "", t(i18n.VerbSchedules)},
		{string(protocol.CommandGrant), "<project> <who> <role>", t(i18n.VerbGrant)},
		{string(protocol.CommandApprove), "<id>", t(i18n.VerbApprove)},
		{string(protocol.CommandDeny), "<id>", t(i18n.VerbDeny)},
		{string(protocol.CommandEffects), "[id happened|new]", t(i18n.VerbEffects)},
	}
}

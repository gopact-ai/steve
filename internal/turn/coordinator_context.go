package turn

import (
	"context"
	"errors"
	"sort"

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
}

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
		binding, err := c.bindingFor(ctx, Request{ConversationID: conversationID})
		if err != nil {
			var user UserError
			if !errors.As(err, &user) {
				return out, err
			}
		} else if p, ok, err := c.projects.Get(ctx, binding.ProjectID); err == nil && ok {
			current = &p
			out.Project = &ContextProject{
				ID: p.ID, Node: nodewire.Place(p.Home.Node), Path: p.Home.Path,
				Level: string(p.Level.OrDefault()), Repo: string(p.Repo), Version: binding.Version,
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

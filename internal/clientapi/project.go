package clientapi

import (
	"strconv"
	"strings"

	"github.com/gopact-ai/steve/internal/conversationapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// threadPrefix is the namespace of threads clients act in.
const threadPrefix = "console:"

// threadID is the console's canonical form of a client-chosen thread ID.
func threadID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return threadPrefix + "main"
	}
	if !strings.HasPrefix(id, threadPrefix) {
		return threadPrefix + id
	}
	return id
}

// turnState folds the console's exchange states into the protocol's five:
// recovering and awaiting-user are running, with the detail as phase.
func turnState(state conversationapi.ExchangeState) (TurnState, string) {
	switch state {
	case conversationapi.ExchangeQueued:
		return TurnQueued, ""
	case conversationapi.ExchangeDone:
		return TurnDone, ""
	case conversationapi.ExchangeFailed:
		return TurnFailed, ""
	case conversationapi.ExchangeCancelled:
		return TurnCancelled, ""
	case conversationapi.ExchangeRunning:
		return TurnRunning, ""
	}
	return TurnRunning, string(state)
}

func toTurn(e conversationapi.Exchange) Turn {
	state, phase := turnState(e.State)
	return Turn{
		ID: e.ID, Thread: e.Conversation, State: state, Phase: phase, Input: e.Input,
		CommandID: strings.TrimPrefix(e.Key, "client:"), ExpectedProject: e.ExpectedProject, Origin: e.Origin,
		QueuedAt: e.EnqueuedAt, StartedAt: e.StartedAt, ReplyItem: e.ReplyID,
	}
}

func toThread(c conversationapi.Conversation) Thread {
	transport := c.Transport
	if transport == "" {
		transport = "console"
	}
	return Thread{
		ID: c.ID, Title: c.Title, Transport: transport, ReadOnly: c.ReadOnly || transport != "console",
		Project: c.Project, Agent: c.Agent, Running: c.Running, Archived: c.Archived,
		PendingQuestions: c.Questions, Lines: c.Count, UpdatedAt: c.LastAt,
	}
}

// toItems expands one transcript line into the items it holds: a sent line
// is the user's message; a reply carries its reasoning, the tools and steps
// it used and what it changed before the answer itself. The answer keeps
// the line's ID, so a turn's reply_item names it; the others derive theirs.
func toItems(r conversationapi.Reply) []Item {
	if r.Silent {
		return nil
	}
	base := Item{ID: r.ID, Thread: r.Conversation, Turn: r.ExchangeID, At: r.At, Attempt: r.AttemptID}
	switch r.Kind {
	case "sent":
		item := base
		item.Kind, item.Role, item.Text, item.Relayed = ItemMessage, "user", r.Input, r.Relayed
		return []Item{item}
	case "milestone", "notice":
		item := base
		item.Kind, item.Text, item.Format, item.Title, item.Error = ItemKind(r.Kind), r.Text, r.Format, r.Title, r.Error
		return []Item{item}
	}
	var out []Item
	derived := func(suffix string, kind ItemKind) Item {
		item := base
		item.ID, item.Kind = r.ID+"#"+suffix, kind
		return item
	}
	if p := r.Process; p != nil {
		if p.Reasoning != "" {
			item := derived("reasoning", ItemReasoning)
			item.Text = p.Reasoning
			out = append(out, item)
		}
		for i, tool := range p.Tools {
			item := derived("tool-"+strconv.Itoa(i), ItemToolCall)
			item.Tool = toToolCall(tool)
			out = append(out, item)
		}
		for _, step := range p.Steps {
			item := derived("step-"+step.ID, ItemToolCall)
			item.Tool = &ToolCall{ID: step.ID, Kind: orDefault(step.Kind, "step"), Name: step.Goal, Status: string(step.State), Output: step.Answer, Agent: step.Agent, Node: step.Node, Task: step.ID}
			out = append(out, item)
		}
	}
	if c := r.Changes; c != nil {
		item := derived("change", ItemFileChange)
		item.Change = &FileChange{Attempt: c.Attempt, Project: c.Project, Files: c.Files, Added: c.Added, Deleted: c.Deleted, Truncated: c.Truncated, Note: c.Note}
		out = append(out, item)
	}
	item := base
	item.Kind, item.Role, item.Text, item.Format, item.Title, item.Error = ItemMessage, "assistant", r.Text, r.Format, r.Title, r.Error
	return append(out, item)
}

func toToolCall(t conversationapi.ToolCall) *ToolCall {
	return &ToolCall{ID: t.ID, Kind: t.Kind, Name: t.Name, Detail: t.Detail, Status: string(t.Status), Input: t.Input, Output: t.Output}
}

func toQuestion(q conversationapi.PendingQuestion) Question {
	out := Question{
		ID: q.ID, Thread: q.Conversation, Turn: q.ExchangeID, Project: q.Project, Task: q.TaskID, Attempt: q.AttemptID,
		Agent: q.Agent, Node: q.Node, Kind: q.Kind, Title: q.Title, Message: q.Message, Options: []QuestionOption{},
		AllowFreeText: q.AllowFreeText, Required: q.Required, State: q.State, CreatedAt: q.CreatedAt, Deadline: q.Deadline,
	}
	for _, o := range q.Options {
		out.Options = append(out.Options, QuestionOption{ID: o.ID, Label: o.Label, Description: o.Description, Kind: o.Kind})
	}
	if a := q.Answer; a != nil {
		out.Answer = &QuestionAnswer{CommandID: a.CommandID, Decision: a.Decision, Choice: a.Choice, Text: a.Text}
	}
	return out
}

func questionItem(q conversationapi.PendingQuestion) Item {
	question := toQuestion(q)
	return Item{ID: "question:" + q.ID, Kind: ItemQuestion, Thread: q.Conversation, Turn: q.ExchangeID, At: q.CreatedAt, Attempt: q.AttemptID, Text: q.Message, Title: q.Title, Question: &question}
}

func toProgress(p *conversationapi.Progress) *Progress {
	if p == nil {
		return nil
	}
	out := &Progress{Phase: p.Phase, Agent: p.Agent, Node: p.Node, Model: p.Model, Reasoning: p.Reasoning, Answer: p.Answer}
	for _, t := range p.Tools {
		out.Tools = append(out.Tools, *toToolCall(t))
	}
	for _, l := range p.Plan {
		out.Plan = append(out.Plan, PlanLine(l))
	}
	return out
}

func stepProgress(s *conversationapi.StepProcess) *Progress {
	if s == nil {
		return nil
	}
	out := &Progress{Agent: s.Agent, Node: s.Node, Model: s.Model, Reasoning: s.Reasoning, Answer: s.Answer}
	for _, t := range s.Tools {
		out.Tools = append(out.Tools, *toToolCall(t))
	}
	for _, l := range s.Plan {
		out.Plan = append(out.Plan, PlanLine(l))
	}
	return out
}

// toEvent names a read-model event in the protocol's terms. Kinds the
// protocol has no name for travel as server.event with their own kind.
func toEvent(cursor string, ev readmodel.Event) Event {
	out := Event{Cursor: cursor, Type: EventServer, At: ev.At, Thread: ev.Conversation, Turn: ev.ExchangeID, Task: ev.TaskID, State: ev.State, Silent: ev.Silent, SourceKind: ev.Kind}
	switch ev.Kind {
	case "console.sent", "console.reply", "console.milestone", "console.notice":
		out.Type, out.Item, out.Text = EventItemCompleted, ev.ReplyID, ev.Text
	case "console.recalled":
		out.Type, out.Item = EventItemRemoved, ev.ReplyID
	case "console.queue":
		out.Type = EventTurnUpdated
	case "console.progress":
		out.Type, out.Progress = EventTurnProgress, toProgress(ev.Progress)
	case "console.step":
		out.Type, out.Item, out.Progress = EventTurnProgress, ev.ReplyID, stepProgress(ev.Step)
		if ev.Step != nil {
			out.Task = ev.Step.ID
		}
	case "console.question":
		out.Type, out.Question = EventQuestionUpdated, ev.Text
	case "console.meta":
		out.Type, out.Text = EventThreadUpdated, ev.Text
	case "console.deleted":
		out.Type = EventThreadDeleted
	case "task.changed":
		out.Type = EventTaskUpdated
	default:
		out.Text = ev.Text
	}
	return out
}

func toProject(p readmodel.Project) Project {
	agents := p.Agents
	if agents == nil {
		agents = []string{}
	}
	return Project{ID: p.ID, Node: p.Node, Path: p.Path, Repo: p.Repo, Level: p.Level, Home: p.Home, Default: p.Default, Agents: agents}
}

func toAgent(a readmodel.Agent) Agent {
	model := a.Model
	if model == "" {
		model = a.Preferred
	}
	return Agent{ID: a.ID, Node: a.Node, Harness: a.Harness, Model: model, Ready: a.Eligible, Why: a.Why, About: a.About, Default: a.Default, Busy: a.Busy}
}

func toTask(t readmodel.Task) Task {
	return Task{
		ID: t.ID, Title: t.Title, Goal: t.Goal, State: string(t.State), Thread: t.Channel, Transport: t.Transport,
		Project: t.ProjectID, Parent: t.Parent, Agent: t.Member, Node: t.NodeID, Origin: t.Origin,
		Children: t.ChildrenCount, UpdatedAt: t.UpdatedAt,
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

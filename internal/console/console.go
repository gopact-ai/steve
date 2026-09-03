// Package console is the owner acting from the web page: the same verbs
// the chat has, through the same coordinator, as the same principal. A
// console conversation is "console:<name>"; its anchors are not Feishu
// messages, so what would have been a card or a milestone in the chat is
// kept here and pushed to the page over the change stream.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

// Prefix marks a console conversation and its message ids.
const (
	Prefix     = "console:"
	ChatID     = "console"
	AnchorMark = "web-"
	keep       = 200
)

type outcome struct {
	reply readmodel.Reply
	err   error
}

// ErrCommandRunning says the same command id is being handled right now.
var ErrCommandRunning = errors.New("this command is already running")

// Handler is the coordinator's door.
type Handler interface {
	Handle(ctx context.Context, req turn.Request) (turn.Result, error)
}

type Service struct {
	handler Handler
	owner   string
	model   *readmodel.Model

	mu      sync.Mutex
	replies map[string][]readmodel.Reply
	// commands remembers each command id's answer; inflight guards a
	// command still running.
	commands     map[string]outcome
	commandOrder []string
	inflight     map[string]bool
	// doc keeps the transcript across restarts. A console whose history
	// vanishes with the process would make every restart look like the
	// owner had never said anything.
	doc ledger.Doc
}

func New(handler Handler, owner string, model *readmodel.Model) *Service {
	return &Service{handler: handler, owner: owner, model: model, replies: map[string][]readmodel.Reply{}}
}

// Persist keeps the transcript in a durable document and loads what an
// earlier process left there.
func (s *Service) Persist(doc ledger.Doc) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return fmt.Errorf("console: load transcript: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok && len(raw) > 0 {
		var saved map[string][]readmodel.Reply
		if err := json.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("console: transcript is not readable: %w", err)
		}
		for conversation, list := range saved {
			s.replies[conversation] = append(list, s.replies[conversation]...)
		}
	}
	s.doc = doc
	return nil
}

// Conversations lists every console conversation with a transcript.
func (s *Service) Conversations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.replies))
	for name := range s.replies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Context is where a conversation stands, from the coordinator's own rules.
func (s *Service) Context(ctx context.Context, conversation string) (readmodel.Context, error) {
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	aware, ok := s.handler.(interface {
		Context(ctx context.Context, conversationID string) (turn.Context, error)
	})
	if !ok {
		return readmodel.Context{Conversation: conversation, Agents: []readmodel.AgentChoice{}}, nil
	}
	got, err := aware.Context(ctx, conversation)
	if err != nil {
		return readmodel.Context{}, err
	}
	out := readmodel.Context{Conversation: conversation, Agents: []readmodel.AgentChoice{}}
	if got.Project != nil {
		out.Project = &readmodel.ContextProject{ID: got.Project.ID, Node: got.Project.Node, Path: got.Project.Path, Level: got.Project.Level, Repo: got.Project.Repo, Version: got.Project.Version, Bound: got.Project.Bound}
	}
	convert := func(a turn.AgentChoice) readmodel.AgentChoice {
		return readmodel.AgentChoice{ID: a.ID, Node: a.Node, Harness: a.Harness, Model: a.Model, Ready: a.Ready, Why: a.Why, Usable: a.Usable, Because: a.Because, Current: a.Current}
	}
	if got.Agent != nil {
		current := convert(*got.Agent)
		out.Agent = &current
	}
	for _, a := range got.Agents {
		out.Agents = append(out.Agents, convert(a))
	}
	return out, nil
}

// Suggest completes a line by the coordinator's rules.
func (s *Service) Suggest(ctx context.Context, conversation, line string) []readmodel.Suggestion {
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	aware, ok := s.handler.(interface {
		Suggest(ctx context.Context, conversationID, line string) []turn.Suggestion
	})
	if !ok {
		return nil
	}
	var out []readmodel.Suggestion
	for _, x := range aware.Suggest(ctx, conversation, line) {
		out = append(out, readmodel.Suggestion{Label: x.Label, Args: x.Args, Detail: x.Detail, Insert: x.Insert, Muted: x.Muted})
	}
	return out
}

// Verbs is what the console can be told, with help, from the coordinator.
func (s *Service) Verbs() []readmodel.Verb {
	aware, ok := s.handler.(interface{ Verbs() []turn.Verb })
	if !ok {
		return nil
	}
	var out []readmodel.Verb
	for _, v := range aware.Verbs() {
		out = append(out, readmodel.Verb{Command: v.Command, Args: v.Args, Summary: v.Summary})
	}
	return out
}

// IsConsole says whether an anchor or conversation belongs to the page.
func IsConsole(conversationOrAnchor string) bool {
	return strings.HasPrefix(conversationOrAnchor, Prefix) || strings.HasPrefix(conversationOrAnchor, AnchorMark)
}

// Send runs one line as the owner and records the answer, and while the
// line runs, what is being done for it: the agent's reasoning and tool
// calls stream to the page as console.progress, a plan's steps report
// theirs as step.progress, and the reply keeps the whole process so it
// can be unfolded later.
func (s *Service) Send(ctx context.Context, conversation, input string) (readmodel.Reply, error) {
	return s.SendCommand(ctx, conversation, input, "")
}

// SendCommand is Send with an idempotency key: a page that retries, a
// double click, a second tab — the same command id gets the first
// answer back and nothing runs twice.
func (s *Service) SendCommand(ctx context.Context, conversation, input, commandID string) (reply readmodel.Reply, err error) {
	if commandID != "" {
		s.mu.Lock()
		if done, ok := s.commands[commandID]; ok {
			s.mu.Unlock()
			return done.reply, done.err
		}
		if s.inflight[commandID] {
			s.mu.Unlock()
			return readmodel.Reply{}, ErrCommandRunning
		}
		if s.inflight == nil {
			s.inflight = map[string]bool{}
		}
		s.inflight[commandID] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, commandID)
			if s.commands == nil {
				s.commands = map[string]outcome{}
			}
			s.commands[commandID] = outcome{reply: reply, err: err}
			s.commandOrder = append(s.commandOrder, commandID)
			if len(s.commandOrder) > keep {
				delete(s.commands, s.commandOrder[0])
				s.commandOrder = s.commandOrder[1:]
			}
			s.mu.Unlock()
		}()
	}
	if s.owner == "" {
		return readmodel.Reply{}, fmt.Errorf("the console needs feishu.owner_open_id: it acts as the owner")
	}
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	id := fmt.Sprintf("%s%d", AnchorMark, time.Now().UnixNano())
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Input: input, Kind: "sent"})

	work := newProcess()
	stop := s.follow(ctx, conversation, work)
	result, err := s.handler.Handle(ctx, turn.Request{
		ConversationID: conversation, ChatID: ChatID, MessageID: id, Input: input,
		SenderOpenID: s.owner, ChatType: protocol.ChatP2P, Mentioned: true,
		OnProgress: s.progress(conversation, work),
	})
	stop()
	reply = readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Title: result.Title, Text: result.Text, Kind: "reply", Process: work.summary()}
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = err.Error()
		}
	}
	s.record(reply)
	return reply, err
}

// progress is the turn's own stream, published no more often than a page
// can usefully repaint, and kept as the reply's process.
func (s *Service) progress(conversation string, work *process) func(view.Progress) {
	var mu sync.Mutex
	var last time.Time
	return func(p view.Progress) {
		cut := readmodel.FromProgress(p)
		work.turn(cut)
		if s.model == nil {
			return
		}
		mu.Lock()
		due := time.Since(last) >= progressEvery || toolsChanged(work, cut)
		if due {
			last = time.Now()
		}
		mu.Unlock()
		if due {
			s.model.Publish(readmodel.Event{Kind: "console.progress", Conversation: conversation, Progress: &cut})
		}
	}
}

// progressEvery bounds how often a token stream repaints the page.
const progressEvery = 500 * time.Millisecond

func toolsChanged(work *process, next readmodel.Progress) bool {
	work.mu.Lock()
	defer work.mu.Unlock()
	if len(next.Tools) != work.publishedTools {
		work.publishedTools = len(next.Tools)
		return true
	}
	return false
}

// follow collects the steps' progress for a plan run on this conversation
// while the line runs. The read model stamps step.progress with the
// conversation, so this is the same stream the page watches.
func (s *Service) follow(ctx context.Context, conversation string, work *process) func() {
	if s.model == nil {
		return func() {}
	}
	events, stop := s.model.Subscribe(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Kind == "step.progress" && ev.Conversation == conversation && ev.Progress != nil {
				work.step(ev.StepID, *ev.Progress)
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

// process is what one console line caused, gathered as it happens.
type process struct {
	mu             sync.Mutex
	last           readmodel.Progress
	steps          map[string]readmodel.StepProcess
	order          []string
	publishedTools int
}

func newProcess() *process { return &process{steps: map[string]readmodel.StepProcess{}} }

func (w *process) turn(p readmodel.Progress) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = p
}

func (w *process) step(id string, p readmodel.Progress) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, seen := w.steps[id]; !seen {
		w.order = append(w.order, id)
	}
	w.steps[id] = readmodel.StepProcess{ID: id, Agent: p.Agent, Node: p.Node, Reasoning: p.Reasoning, Tools: p.Tools}
}

// summary is the process as the reply keeps it, or nil when nothing was
// observed — a verb answered from state has no process worth a fold.
func (w *process) summary() *readmodel.Process {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := &readmodel.Process{Reasoning: w.last.Reasoning, Tools: w.last.Tools}
	for _, id := range w.order {
		out.Steps = append(out.Steps, w.steps[id])
	}
	if out.Reasoning == "" && len(out.Tools) == 0 && len(out.Steps) == 0 {
		return nil
	}
	return out
}

// Replies is a conversation's recent exchanges, oldest first.
func (s *Service) Replies(conversation string) []readmodel.Reply {
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readmodel.Reply{}, s.replies[conversation]...)
}

// Notice takes a task notice whose anchor is the console — a resumed
// plan's outcome, an approved disclosure — and shows it on the page.
func (s *Service) Notice(n turn.TaskNotice) {
	conversation := Prefix + "main"
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Title: "task #" + n.TaskID, Text: n.Text, Kind: "notice"})
}

// Milestone is what an agent's feishu_send becomes on the console.
func (s *Service) Milestone(anchor, text string) string {
	conversation := Prefix + "main"
	id := fmt.Sprintf("%s%d", AnchorMark, time.Now().UnixNano())
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Text: text, Kind: "milestone"})
	return id
}

func (s *Service) record(r readmodel.Reply) {
	s.mu.Lock()
	list := append(s.replies[r.Conversation], r)
	if len(list) > keep {
		list = list[len(list)-keep:]
	}
	s.replies[r.Conversation] = list
	if s.doc != nil {
		// The whole transcript is small (keep entries per conversation);
		// one durable replace is simpler than a log to compact.
		if raw, err := json.Marshal(s.replies); err == nil {
			if err := s.doc.Save(raw); err != nil {
				log.Printf("console: save transcript: %v", err)
			}
		}
	}
	s.mu.Unlock()
	if s.model != nil {
		text := r.Text
		if r.Kind == "sent" {
			text = r.Input
		}
		s.model.Publish(readmodel.Event{At: r.At, Kind: "console." + r.Kind, Conversation: r.Conversation, Text: text, Title: r.Title})
	}
}

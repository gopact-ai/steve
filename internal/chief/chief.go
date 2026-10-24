// Package chief is Steve as the owner meets it: the chief of staff in the
// main thread. Each owner message starts a turn (messages that arrive during
// a turn join the next one). A turn reads the recent conversation and the
// task bar, answers, and proposes actions — tasks to start, tasks to stop —
// which the program checks and carries out. When a task ends, the program,
// not a model, posts the outcome back to the thread.
package chief

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/thread"
	"github.com/gopact-ai/steve/internal/work"
)

// Runner runs one prompt as an attempt of a task.
type Runner interface {
	Prompt(ctx context.Context, spec agentexec.Spec, prompt string, validate func(string) error) (agentexec.Result, error)
}

// Work is what the chief does with tasks.
type Work interface {
	Submit(ctx context.Context, r work.Request) (work.Card, error)
	Stop(ctx context.Context, id string) (work.Card, error)
	List() []work.Card
	OnFinished(func(work.Card))
}

type Options struct {
	Agent   string        // the agent the chief runs as
	Project string        // where its read-only worktree comes from
	Locale  string        // zh or en: the language it answers and notices in
	Timeout time.Duration // bound on one turn
	History int           // messages of context per turn
}

type Chief struct {
	thread *thread.Service
	work   Work
	tasks  *task.Store
	runner Runner
	opts   Options
	ctx    context.Context

	catalog i18n.Catalog

	mu      sync.Mutex
	pending bool
	busy    bool
	idle    *sync.Cond
}

const origin = "chief"

// IsTurn says whether a task is one of the chief's own turns. A turn only
// proposes; what it asks for while running is refused, and it asks the
// owner in its reply instead.
func IsTurn(t task.Task) bool { return t.Origin == origin }

func New(ctx context.Context, threads *thread.Service, w Work, tasks *task.Store, runner Runner, opts Options) *Chief {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.History <= 0 {
		opts.History = 40
	}
	if opts.Locale != "en" {
		opts.Locale = "zh"
	}
	c := &Chief{thread: threads, work: w, tasks: tasks, runner: runner, opts: opts, ctx: ctx, catalog: i18n.New(i18n.FromLang(opts.Locale))}
	c.idle = sync.NewCond(&c.mu)
	threads.OnPosted(func(m thread.Message) {
		if m.Role == thread.RoleOwner && m.Thread == thread.Main {
			c.kick()
		}
	})
	w.OnFinished(c.notice)
	return c
}

// kick asks for a turn; a turn already running picks the message up next.
func (c *Chief) kick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = true
	if c.busy {
		return
	}
	c.busy = true
	go c.loop()
}

func (c *Chief) loop() {
	for {
		c.mu.Lock()
		if !c.pending || c.ctx.Err() != nil {
			c.busy = false
			c.idle.Broadcast()
			c.mu.Unlock()
			return
		}
		c.pending = false
		c.mu.Unlock()
		c.turn()
	}
}

// Wait blocks until no turn is running or queued.
func (c *Chief) Wait() {
	c.mu.Lock()
	for c.busy {
		c.idle.Wait()
	}
	c.mu.Unlock()
}

func (c *Chief) turn() {
	ctx := c.ctx
	history, err := c.thread.Recent(ctx, thread.Main, c.opts.History)
	if err != nil || len(history) == 0 {
		return
	}
	turnTask, err := c.tasks.CreateChecked(ctx, task.Task{Goal: "reply to the owner", Member: c.opts.Agent, ProjectID: c.opts.Project, Channel: thread.Main, Origin: origin, System: true, Transport: "chief"}, nil)
	if err != nil {
		c.say(ctx, c.unavailable(err))
		return
	}
	token, err := c.tasks.ExecutionToken(turnTask.ID)
	if err == nil {
		_, err = c.tasks.AdvanceExecution(token, task.StateRunning)
	}
	if err != nil {
		c.say(ctx, c.unavailable(err))
		return
	}
	out, err := c.runner.Prompt(ctx, agentexec.Spec{TaskID: turnTask.ID, TurnID: "chief/" + turnTask.ID, Agent: c.opts.Agent, Project: c.opts.Project, Kind: attempt.KindPlan, Timeout: c.opts.Timeout}, c.prompt(history), nil)
	final := task.StateDone
	if err != nil {
		final = task.StateFailed
	}
	if _, advanceErr := c.tasks.AdvanceExecution(token, final); advanceErr != nil && !errors.Is(advanceErr, task.ErrExecutionStopped) {
		slog.Warn("chief: settle turn", "task", turnTask.ID, "error", advanceErr)
	}
	if err != nil {
		c.say(ctx, c.unavailable(err))
		return
	}
	reply, actions, parseErr := parseReply(out.Answer)
	started, notes := c.act(ctx, turnTask.ID, actions)
	if parseErr != nil {
		notes = append(notes, c.text(i18n.ChiefUnparsed, parseErr.Error()))
	}
	if reply != "" || len(started) > 0 {
		if reply == "" {
			reply = c.text(i18n.ChiefStarted)
		}
		if _, _, err := c.thread.Post(ctx, thread.Message{Role: thread.RoleSteve, Text: reply, Tasks: started}); err != nil {
			slog.Warn("chief: post reply", "error", err)
		}
	}
	for _, note := range notes {
		c.notify(ctx, note, "")
	}
}

// act carries out what the turn proposed, and reports what it could not.
func (c *Chief) act(ctx context.Context, turnID string, a Actions) (started []string, notes []string) {
	for i, d := range a.Delegate {
		card, err := c.work.Submit(ctx, work.Request{Key: fmt.Sprintf("chief:%s:%d", turnID, i), Goal: d.Goal, Agent: d.Agent, Mode: d.Mode, Thread: thread.Main, By: "chief"})
		if err != nil {
			notes = append(notes, c.text(i18n.ChiefNotStarted, clip(d.Goal, 80)+": "+err.Error()))
			continue
		}
		started = append(started, card.ID)
	}
	for _, id := range a.Stop {
		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := c.work.Stop(stopCtx, id)
		cancel()
		if err != nil {
			notes = append(notes, c.text(i18n.ChiefNotStopped, "#"+id+": "+err.Error()))
		}
	}
	return started, notes
}

// notice posts a task's outcome to the thread it belongs to.
func (c *Chief) notice(card work.Card) {
	if card.Thread != thread.Main {
		return
	}
	var key i18n.Key
	switch card.State {
	case work.StateDone:
		key = i18n.ChiefTaskDone
	case work.StateStopped:
		key = i18n.ChiefTaskStopped
	case work.StateFailed:
		key = map[string]i18n.Key{work.FailQuota: i18n.ChiefTaskFailedQuota, work.FailTransport: i18n.ChiefTaskFailedTransport}[card.Failure]
		if key == "" {
			key = i18n.ChiefTaskFailed
		}
	default:
		return
	}
	text := c.text(key, card.ID, clip(card.Goal, 60))
	if body := strings.TrimSpace(card.Answer + card.Error); body != "" {
		text += "\n\n" + clip(body, 1500)
	}
	c.notify(c.ctx, text, card.ID)
}

func (c *Chief) say(ctx context.Context, text string) {
	if _, _, err := c.thread.Post(ctx, thread.Message{Role: thread.RoleNotice, Text: text}); err != nil {
		slog.Warn("chief: post", "error", err)
	}
}

func (c *Chief) notify(ctx context.Context, text, taskID string) {
	if _, _, err := c.thread.Post(ctx, thread.Message{Role: thread.RoleNotice, Text: text, TaskID: taskID}); err != nil {
		slog.Warn("chief: notice", "error", err)
	}
}

func (c *Chief) prompt(history []thread.Message) string {
	var b strings.Builder
	language := "Chinese (简体中文)"
	if c.opts.Locale == "en" {
		language = "English"
	}
	fmt.Fprintf(&b, `You are Steve, the owner's chief of staff. You do not do the work yourself: you understand what the owner wants, hand concrete work to executors as tasks, follow up, and report. Be brief and concrete. Reply in %s.

Tasks on the task bar (newest first):
`, language)
	cards := c.work.List()
	if len(cards) == 0 {
		b.WriteString("(none)\n")
	}
	for i, card := range cards {
		if i == 20 {
			fmt.Fprintf(&b, "... %d older\n", len(cards)-20)
			break
		}
		fmt.Fprintf(&b, "#%s [%s, %s] %s", card.ID, card.State, card.Mode, clip(card.Goal, 120))
		if card.Activity.Line != "" {
			fmt.Fprintf(&b, " — now: %s", card.Activity.Line)
		}
		if card.Answer != "" {
			fmt.Fprintf(&b, " — result: %s", clip(firstLine(card.Answer), 160))
		}
		if card.Error != "" {
			fmt.Fprintf(&b, " — error (%s): %s", card.Failure, clip(firstLine(card.Error), 160))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nConversation (oldest first):\n")
	for _, m := range history {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Text)
	}
	b.WriteString("\nAnswer the owner's latest message(s). If work should start or stop, end your reply with exactly one block:\n\n```" + fence + `
{"delegate": [{"goal": "<complete, self-contained instructions>", "mode": "auto"}], "stop": ["<task number>"]}
` + "```" + `

- delegate: one entry per independent piece of work. The executor sees nothing of this conversation, so the goal must name the repository, the issue or file, and what done looks like.
- mode: "auto" (default: the executor asks the owner before risky steps) or "yolo" (only when the owner asked for it).
- stop: numbers of tasks the owner asked to stop.
Leave the block out when nothing should start or stop. Do not say a task has started unless it is in the block; the program confirms what actually started. For questions about progress, answer from the task bar above.`)
	return b.String()
}

func (c *Chief) text(key i18n.Key, args ...any) string { return c.catalog.T(key, args...) }

// unavailable says a turn could not reply, naming the class of failure.
func (c *Chief) unavailable(err error) string {
	class := ""
	switch work.Classify(err) {
	case work.FailQuota:
		class = c.text(i18n.ChiefClassQuota)
	case work.FailTransport:
		class = c.text(i18n.ChiefClassTransport)
	}
	return c.text(i18n.ChiefUnavailable, class, clip(err.Error(), 300))
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

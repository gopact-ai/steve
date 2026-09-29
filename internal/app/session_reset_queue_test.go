package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

const typedAheadConversation = "console:main"

// freshSessions is an agent on the hub that starts a session with an id of
// its own whenever no earlier one is resumed, and records which session
// each prompt reached.
type freshSessions struct {
	mu      sync.Mutex
	opened  int
	reached map[string]string
}

func (s *freshSessions) OpenSession(_ context.Context, _ harness.Placement, upstreamID, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if upstreamID == "" {
		s.opened++
		upstreamID = fmt.Sprintf("native-%d", s.opened)
	}
	return &freshRunner{id: upstreamID, sessions: s}, nil
}

func (*freshSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

func (*freshSessions) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, nil
}

// sessionOf is the session the prompt carrying line reached, or "".
func (s *freshSessions) sessionOf(line string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for prompt, session := range s.reached {
		if strings.Contains(prompt, line) {
			return session
		}
	}
	return ""
}

type freshRunner struct {
	id       string
	sessions *freshSessions
}

func (r *freshRunner) ID() string { return r.id }

func (r *freshRunner) Prompt(_ context.Context, prompt string, _ func(view.Progress)) (string, []string, error) {
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if r.sessions.reached == nil {
		r.sessions.reached = map[string]string{}
	}
	r.sessions.reached[prompt] = r.id
	return "noted", nil, nil
}

func (*freshRunner) Cancel(context.Context) error { return nil }
func (*freshRunner) Abort()                       {}

// holdCommand keeps the console's turn for one command line from reaching
// the coordinator until it is released, so a line can be typed behind it.
type holdCommand struct {
	*turn.Coordinator
	command string
	entered chan struct{}
	release chan struct{}
}

func (g *holdCommand) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if req.Input == g.command {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	return g.Coordinator.Handle(ctx, req)
}

type typedAhead struct {
	tasks   *task.Store
	cons    *console.Service
	agent   *freshSessions
	command *holdCommand
}

// openTypedAhead wires the real console to a coordinator with the
// production channels and callbacks and an agent on the hub, in a
// conversation bound to project p that may switch to p2. The console's
// turn for command waits until it is released.
func openTypedAhead(t *testing.T, command string) typedAhead {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}, {ID: "p2", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	f := typedAhead{tasks: tasks, agent: &freshSessions{}}
	coordinator := turntest.Unwired(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Runtime, o.Timeout = book, catalog, sessions, f.agent, 10*time.Second
		o.Tasks, o.Node, o.Executions = tasks, "hub", execution.New(t.Context(), tasks)
		o.Projects, o.DefaultProject = projects, "p"
		o.ConsoleCompletionGuard = console.CheckTaskCompletionTx
	})
	f.command = &holdCommand{Coordinator: coordinator, command: command, entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.cons = console.New(f.command, "owner", nil)
	if err := f.cons.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.cons.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	workers := &reconciliationWorkers{}
	t.Cleanup(workers.Close)
	channels, err := assembleChannels(
		&runtimeValues{book: book, cfg: &config.Config{}, ctx: t.Context()},
		&ledgerValues{},
		&executionValues{coordinator: coordinator, gw: gateway.New(coordinator), catalogText: i18n.New(i18n.LocaleEN)},
		&readModelValues{}, &consoleValues{cons: f.cons, reconciliations: workers},
		&administrationValues{}, &delegationValues{},
	)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Wire(turntest.Callbacks(coordinatorCallbacks(nil, nil, messagingCallbacks{}, channels.Routes())))
	return f
}

// holding is the one task the worker holds in the conversation.
func (f typedAhead) holding(t *testing.T) task.Task {
	t.Helper()
	held := f.tasks.Holding(typedAheadConversation, "worker")
	if len(held) != 1 {
		t.Fatalf("worker holds %+v, want one task", held)
	}
	return held[0]
}

// ran waits until the exchange has run and returns it with the text of its
// answer.
func (f typedAhead) ran(t *testing.T, id string) (consoleapi.Exchange, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, e := range f.cons.Queue(typedAheadConversation) {
			if e.ID != id || !e.State.Terminal() {
				continue
			}
			for _, reply := range f.cons.Replies(typedAheadConversation) {
				if reply.ID == e.ReplyID {
					return e, reply.Text
				}
			}
			return e, ""
		}
		if time.Now().After(deadline) {
			t.Fatalf("exchange %s never ran: %+v", id, f.cons.Queue(typedAheadConversation))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs a first turn in the conversation and returns its task and the
// session it reached.
func (f typedAhead) start(t *testing.T) (task.Task, string) {
	t.Helper()
	if _, err := f.cons.SendCommand(t.Context(), typedAheadConversation, "first goal", "first"); err != nil {
		t.Fatal(err)
	}
	session := f.agent.sessionOf("first goal")
	if session == "" {
		t.Fatal("the first goal reached no session")
	}
	return f.holding(t), session
}

// send runs the held command with line typed behind it while it runs, or
// sent once it has answered, and returns the line's exchange. It wants the
// command to go ahead.
func (f typedAhead) send(t *testing.T, line string, ahead bool) consoleapi.Exchange {
	t.Helper()
	command := f.command.command
	if !ahead {
		close(f.command.release)
		if reply, err := f.cons.SendCommand(t.Context(), typedAheadConversation, command, "command"); err != nil {
			t.Fatalf("%s = %+v, %v; want it to go ahead", command, reply, err)
		}
		e, err := f.cons.EnqueueCommand(t.Context(), typedAheadConversation, line, "line", nil)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	type outcome struct {
		reply consoleapi.Reply
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		reply, err := f.cons.SendCommand(context.Background(), typedAheadConversation, command, "command")
		done <- outcome{reply, err}
	}()
	select {
	case <-f.command.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never started", command)
	}
	e, err := f.cons.EnqueueCommand(t.Context(), typedAheadConversation, line, "line", nil)
	if err != nil || e.State != consoleapi.ExchangeQueued {
		t.Fatalf("line typed behind %s = %+v, %v; want it queued", command, e, err)
	}
	close(f.command.release)
	if got := <-done; got.err != nil {
		t.Fatalf("%s with a line typed behind it = %+v, %v; want it to go ahead", command, got.reply, got.err)
	}
	return e
}

// A line typed in the console while /new is still running queues behind
// it, and is meant for the session the reset starts. The reset goes ahead,
// as it does when the line is sent after it, and the line runs in a new
// session under a new task, not in the ones the reset ended.
func TestSessionResetGoesAheadOfALineTypedBehindIt(t *testing.T) {
	for name, ahead := range map[string]bool{"sent_after": false, "typed_ahead": true} {
		t.Run(name, func(t *testing.T) {
			f := openTypedAhead(t, "/new")
			first, before := f.start(t)
			line := f.send(t, "the next line", ahead)
			if tracked, _ := f.tasks.Get(first.ID); tracked.State != task.StateDone {
				t.Fatalf("task after /new = %s, want done", tracked.State)
			}
			if e, text := f.ran(t, line.ID); e.State != consoleapi.ExchangeDone {
				t.Fatalf("line after /new = %+v %q, want done", e, text)
			}
			next := f.holding(t)
			if next.ID == first.ID || next.ProjectID != "p" {
				t.Fatalf("line after /new ran under task %s in %s; want a new task in p, not %s", next.ID, next.ProjectID, first.ID)
			}
			if after := f.agent.sessionOf("the next line"); after == "" || after == before {
				t.Fatalf("line after /new reached session %q; want a new one, not %q", after, before)
			}
		})
	}
}

// A project switch goes ahead of a line typed behind it too. A line sent
// after the switch runs in a new session under a new task in the new
// project. A line typed before the switch answered was accepted for the
// project the conversation then had, so it runs in neither: not in the
// task and session the switch ended, and not in the new project.
func TestProjectSwitchGoesAheadOfALineTypedBehindIt(t *testing.T) {
	t.Run("sent_after", func(t *testing.T) {
		f := openTypedAhead(t, "/project use p2")
		first, before := f.start(t)
		line := f.send(t, "the next line", false)
		if tracked, _ := f.tasks.Get(first.ID); tracked.State != task.StateDone {
			t.Fatalf("task after the switch = %s, want done", tracked.State)
		}
		if e, text := f.ran(t, line.ID); e.State != consoleapi.ExchangeDone {
			t.Fatalf("line after the switch = %+v %q, want done", e, text)
		}
		next := f.holding(t)
		if next.ID == first.ID || next.ProjectID != "p2" {
			t.Fatalf("line after the switch ran under task %s in %s; want a new task in p2, not %s", next.ID, next.ProjectID, first.ID)
		}
		if after := f.agent.sessionOf("the next line"); after == "" || after == before {
			t.Fatalf("line after the switch reached session %q; want a new one, not %q", after, before)
		}
	})
	t.Run("typed_ahead", func(t *testing.T) {
		f := openTypedAhead(t, "/project use p2")
		first, _ := f.start(t)
		line := f.send(t, "the next line", true)
		if tracked, _ := f.tasks.Get(first.ID); tracked.State != task.StateDone {
			t.Fatalf("task after the switch = %s, want done", tracked.State)
		}
		if e, text := f.ran(t, line.ID); e.State != consoleapi.ExchangeFailed || !strings.Contains(text, "bound to p2") {
			t.Fatalf("line typed for p behind the switch = %+v %q; want it refused for the new binding", e, text)
		}
		if held := f.tasks.Holding(typedAheadConversation, "worker"); len(held) != 0 {
			t.Fatalf("line typed for p behind the switch left the worker holding %+v", held)
		}
		if after := f.agent.sessionOf("the next line"); after != "" {
			t.Fatalf("line typed for p behind the switch reached session %q", after)
		}
	})
}

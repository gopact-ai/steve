package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// sessions is a harness whose prompt is a script.
type sessions struct {
	run func(ctx context.Context, prompt string, progress func(view.Progress)) (string, error)
}

func (s *sessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return &session{s}, nil
}
func (s *sessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type session struct{ s *sessions }

func (*session) ID() string { return "session" }
func (x *session) Prompt(ctx context.Context, prompt string, progress func(view.Progress)) (string, []string, error) {
	answer, err := x.s.run(ctx, prompt, progress)
	return answer, nil, err
}
func (*session) Cancel(context.Context) error { return nil }
func (*session) Abort()                       {}
func (*session) Stopped() bool                { return false }

type world struct {
	svc      *Service
	feed     *feed.Feed
	sessions *sessions
	tasks    *task.Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(catalog)
	fleet.SetHubCapabilities([]string{"basic"})
	ss := &sessions{run: func(context.Context, string, func(view.Progress)) (string, error) { return "done", nil }}
	executions := execution.New(t.Context(), tasks)
	runner := agentexec.New(ss, fleet, artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()}), attempt.New(book), executions, Budget{Tasks: tasks})
	events := feed.New("test", 0)
	svc := New(t.Context(), book, tasks, runner, executions, events, Options{Agent: "codex", Project: "p"})
	runner.SetObserver(svc.Observe)
	t.Cleanup(svc.Wait)
	return &world{svc: svc, feed: events, sessions: ss, tasks: tasks}
}

func (w *world) await(t *testing.T, id string, want ...string) Card {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		card, err := w.svc.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range want {
			if card.State == state {
				return card
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s is %s, want %v: %+v", id, card.State, want, card)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubmittedTaskRunsInItsWorktreeAndEndsDone(t *testing.T) {
	w := newWorld(t)
	var gotPrompt string
	w.sessions.run = func(_ context.Context, prompt string, _ func(view.Progress)) (string, error) {
		gotPrompt = prompt
		return "Opened PR #12", nil
	}
	card, err := w.svc.Submit(t.Context(), Request{Key: "nonce-1", Goal: "fix issue 7"})
	if err != nil {
		t.Fatal(err)
	}
	if card.Agent != "codex" || card.Project != "p" || card.Mode != ModeAuto {
		t.Fatalf("defaults not applied: %+v", card)
	}
	done := w.await(t, card.ID, StateDone, StateFailed)
	if done.State != StateDone || done.Answer != "Opened PR #12" {
		t.Fatalf("card = %+v", done)
	}
	if gotPrompt == "" || !strings.Contains(gotPrompt, "fix issue 7") || !strings.Contains(gotPrompt, "pull request") {
		t.Fatalf("executor prompt = %q", gotPrompt)
	}
	again, err := w.svc.Submit(t.Context(), Request{Key: "nonce-1", Goal: "fix issue 7"})
	if err != nil || again.ID != card.ID {
		t.Fatalf("repeated key created %+v, %v", again, err)
	}
	if n := len(w.svc.List()); n != 1 {
		t.Fatalf("tasks = %d", n)
	}
}

func TestCardActivityComesFromHarnessEventsAndReachesTheFeed(t *testing.T) {
	w := newWorld(t)
	release := make(chan struct{})
	reported := make(chan struct{})
	w.sessions.run = func(ctx context.Context, _ string, progress func(view.Progress)) (string, error) {
		progress(view.Progress{
			Tools: []view.Tool{{Kind: "read", Name: "Read main.go", Status: view.ToolCompleted}, {Kind: "execute", Name: "go test ./...\nmore", Status: view.ToolRunning}},
			Plan:  []view.Step{{Text: "reproduce", Status: view.StepCompleted}, {Text: "fix the parser", Status: "in_progress"}, {Text: "open PR"}},
			Usage: view.Usage{Reported: true, InputTokens: 900, OutputTokens: 100, ContextTokens: 4000, ContextWindow: 200000},
		})
		close(reported)
		<-release
		return "ok", nil
	}
	start := w.feed.Head()
	card, err := w.svc.Submit(t.Context(), Request{Key: "k", Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	<-reported
	running := w.await(t, card.ID, StateRunning)
	a := running.Activity
	if a.Line != "go test ./..." || a.Stage == nil || a.Stage.Done != 1 || a.Stage.Total != 3 || a.Stage.Current != "fix the parser" || a.Tokens != 1000 || a.ContextWindow != 200000 {
		t.Fatalf("activity = %+v stage=%+v", a, a.Stage)
	}
	close(release)
	w.await(t, card.ID, StateDone)
	w.svc.Wait()
	events, err := w.feed.After(start)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		var c Card
		if e.Type != EventTask || json.Unmarshal(e.Data, &c) != nil || c.ID != card.ID {
			t.Fatalf("event %+v", e)
		}
		seen[c.State] = true
		if c.State == StateRunning && c.Activity.Line == "go test ./..." {
			seen["activity"] = true
		}
	}
	for _, want := range []string{StateQueued, StateRunning, "activity", StateDone} {
		if !seen[want] {
			t.Fatalf("feed never showed %s: %v", want, seen)
		}
	}
}

func TestStopEndsTheExecutionAndALateAnswerCannotReviveIt(t *testing.T) {
	w := newWorld(t)
	entered := make(chan struct{})
	w.sessions.run = func(ctx context.Context, _ string, _ func(view.Progress)) (string, error) {
		close(entered)
		<-ctx.Done()
		return "finished anyway", nil
	}
	card, err := w.svc.Submit(t.Context(), Request{Key: "k", Goal: "long job"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := w.svc.Stop(ctx, card.ID); err != nil {
		t.Fatal(err)
	}
	w.svc.Wait()
	if got, _ := w.svc.Get(card.ID); got.State != StateStopped {
		t.Fatalf("stopped task = %+v", got)
	}
}

func TestFailureNamesItsClass(t *testing.T) {
	w := newWorld(t)
	w.sessions.run = func(context.Context, string, func(view.Progress)) (string, error) {
		return "", errors.New("upstream returned 429 Too Many Requests")
	}
	card, err := w.svc.Submit(t.Context(), Request{Key: "k", Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	failed := w.await(t, card.ID, StateFailed, StateDone)
	if failed.State != StateFailed || failed.Failure != FailQuota || !strings.Contains(failed.Error, "429") {
		t.Fatalf("card = %+v", failed)
	}
}

func TestWaitingShowsWhileSomethingIsOpenForThePerson(t *testing.T) {
	w := newWorld(t)
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	w.sessions.run = func(context.Context, string, func(view.Progress)) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "ok", nil
	}
	card, _ := w.svc.Submit(t.Context(), Request{Key: "k", Goal: "g", Mode: ModeYolo})
	<-entered
	w.await(t, card.ID, StateRunning)
	done := w.svc.Waiting(card.ID)
	if got, _ := w.svc.Get(card.ID); got.State != StateWaiting || got.Mode != ModeYolo {
		t.Fatalf("card = %+v", got)
	}
	done()
	done()
	if got, _ := w.svc.Get(card.ID); got.State != StateRunning {
		t.Fatalf("after answer = %+v", got)
	}
	close(release)
	w.await(t, card.ID, StateDone)
}

func TestSubmitRefusesIncompleteRequests(t *testing.T) {
	w := newWorld(t)
	for _, r := range []Request{{Goal: "g"}, {Key: "k"}, {Key: "k", Goal: "g", Mode: "ask"}} {
		if _, err := w.svc.Submit(t.Context(), r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v accepted: %v", r, err)
		}
	}
}

func TestFinishedListenersHearTheFinalCardOnce(t *testing.T) {
	w := newWorld(t)
	heard := make(chan Card, 4)
	w.svc.OnFinished(func(c Card) { heard <- c })
	card, _ := w.svc.Submit(t.Context(), Request{Key: "k", Goal: "g"})
	w.svc.Wait()
	select {
	case c := <-heard:
		if c.ID != card.ID || c.State != StateDone {
			t.Fatalf("heard %+v", c)
		}
	default:
		t.Fatal("not told")
	}
	if len(heard) != 0 {
		t.Fatal("told twice")
	}
}

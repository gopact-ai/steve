// Package work is the task service. A person, the chief of staff or an event
// hands it a goal; it runs the goal as one task on an executor agent in an
// isolated worktree, and publishes every change as a card on the task bar.
//
// What a task is and whether it may still run belongs to the kernel's task
// store; how one execution is admitted, run, stopped and settled belongs to
// agentexec and the lifecycle under it. This package decides what to run,
// with which instructions, and what the person sees.
package work

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
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Approval modes. In auto the executor's model decides when a step needs the
// person and asks through an approval card; in yolo nothing is asked.
const (
	ModeAuto = "auto"
	ModeYolo = "yolo"
)

// Card states, as the task bar shows them.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateWaiting = "waiting" // needs the person: an approval or a question is open
	StateDone    = "done"
	StateFailed  = "failed"
	StateStopped = "stopped"
)

// EventTask is the feed event carrying a whole card.
const EventTask = "task.updated"

// Runner runs one execution of a task.
type Runner interface {
	Prompt(ctx context.Context, spec agentexec.Spec, prompt string, validate func(string) error) (agentexec.Result, error)
}

// Stopper ends running executions of tasks.
type Stopper interface {
	Stop(ids []string, cause error) execution.WaitSet
}

// Request asks for one piece of work. Key makes the request idempotent: the
// same key returns the task it created the first time.
type Request struct {
	Key     string `json:"key"`
	Goal    string `json:"goal"`
	Agent   string `json:"agent,omitempty"`
	Project string `json:"project,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Mode    string `json:"mode,omitempty"`
	By      string `json:"by,omitempty"`
}

// Card is a task as the task bar shows it.
type Card struct {
	ID        string    `json:"id"`
	Goal      string    `json:"goal"`
	Agent     string    `json:"agent"`
	Project   string    `json:"project"`
	Thread    string    `json:"thread,omitempty"`
	Mode      string    `json:"mode"`
	State     string    `json:"state"`
	Activity  Activity  `json:"activity"`
	Answer    string    `json:"answer,omitempty"`
	Error     string    `json:"error,omitempty"`
	Failure   string    `json:"failure,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Options are the service's defaults.
type Options struct {
	Agent   string        // executor used when a request names none
	Project string        // project used when a request names none
	Mode    string        // approval mode used when a request names none
	Timeout time.Duration // bound on one execution
}

type Service struct {
	book    *ledger.Ledger
	tasks   *task.Store
	runner  Runner
	stopper Stopper
	feed    *feed.Feed
	opts    Options

	mu      sync.Mutex
	live    map[string]*liveTask // running in this process
	running sync.WaitGroup
	ctx     context.Context
}

type liveTask struct {
	activity Activity
	waiting  int
	cancel   context.CancelFunc
}

// request is what the task store does not hold about a request.
type request struct {
	TaskID string `json:"task_id"`
	Mode   string `json:"mode"`
	By     string `json:"by,omitempty"`
}

const (
	keyKind     = "work-key"
	requestKind = "work-request"
	origin      = "work"
)

func New(ctx context.Context, book *ledger.Ledger, tasks *task.Store, runner Runner, stopper Stopper, events *feed.Feed, opts Options) *Service {
	if opts.Mode == "" {
		opts.Mode = ModeAuto
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Hour
	}
	return &Service{book: book, tasks: tasks, runner: runner, stopper: stopper, feed: events, opts: opts, live: map[string]*liveTask{}, ctx: ctx}
}

var (
	ErrInvalid  = errors.New("work: invalid request")
	ErrNotFound = errors.New("work: task not found")
)

// Submit creates the task and starts it. A repeated key returns the existing
// task without starting anything.
func (s *Service) Submit(ctx context.Context, r Request) (Card, error) {
	r.Goal = strings.TrimSpace(r.Goal)
	if r.Agent == "" {
		r.Agent = s.opts.Agent
	}
	if r.Project == "" {
		r.Project = s.opts.Project
	}
	if r.Mode == "" {
		r.Mode = s.opts.Mode
	}
	if r.Key == "" || len(r.Key) > 200 || r.Goal == "" || len(r.Goal) > 16<<10 || r.Agent == "" || r.Project == "" || (r.Mode != ModeAuto && r.Mode != ModeYolo) {
		return Card{}, fmt.Errorf("%w: key, goal, agent, project and mode auto|yolo are required", ErrInvalid)
	}
	if existing, found, err := s.byKey(ctx, r.Key); err != nil || found {
		return existing, err
	}
	// The key is claimed in the transaction that creates the task, so two
	// submissions of one key cannot both create one.
	created, err := s.tasks.CreateChecked(ctx, task.Task{Goal: r.Goal, Member: r.Agent, ProjectID: r.Project, Channel: r.Thread, Origin: origin, Requester: r.By, Transport: "work"}, func(tx *ledger.Tx) error {
		_, taken, err := durable.Get[string](tx, keyKind, r.Key)
		if err != nil {
			return err
		}
		if taken {
			return ledger.ErrConflict
		}
		return tx.PutBinding(keyKind, r.Key, "")
	})
	if errors.Is(err, ledger.ErrConflict) {
		if existing, found, err := s.byKey(ctx, r.Key); err != nil || found {
			return existing, err
		}
	}
	if err != nil {
		return Card{}, err
	}
	// The key now names its task; losing this write only costs the mode
	// (auto, the safe default) and a duplicate-key lookup by scan.
	if err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		if err := tx.PutBinding(keyKind, r.Key, created.ID); err != nil {
			return err
		}
		return tx.PutBinding(requestKind, created.ID, request{TaskID: created.ID, Mode: r.Mode, By: r.By})
	}); err != nil {
		slog.Warn("work: record request", "task", created.ID, "error", err)
	}
	s.start(created.ID)
	return s.Get(created.ID)
}

func (s *Service) byKey(ctx context.Context, key string) (Card, bool, error) {
	var id string
	var found bool
	err := s.book.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		id, found, err = durable.Get[string](tx, keyKind, key)
		return err
	})
	if err != nil || !found || id == "" {
		return Card{}, false, err
	}
	card, err := s.Get(id)
	return card, err == nil, err
}

// start runs the task in the background, once.
func (s *Service) start(id string) {
	s.mu.Lock()
	if _, running := s.live[id]; running {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.live[id] = &liveTask{cancel: cancel}
	s.running.Add(1)
	s.mu.Unlock()
	s.publish(id)
	go func() {
		defer s.running.Done()
		defer cancel()
		s.run(ctx, id)
		s.mu.Lock()
		delete(s.live, id)
		s.mu.Unlock()
		s.publish(id)
	}()
}

func (s *Service) run(ctx context.Context, id string) {
	t, ok := s.tasks.Get(id)
	if !ok {
		return
	}
	token, err := s.tasks.ExecutionToken(id)
	if err != nil {
		return
	}
	if _, err := s.tasks.AdvanceExecution(token, task.StateRunning); err != nil {
		slog.Warn("work: start", "task", id, "error", err)
		return
	}
	s.publish(id)
	out, err := s.runner.Prompt(ctx, agentexec.Spec{TaskID: id, TurnID: "work/" + id, Agent: t.Member, Project: t.ProjectID, Kind: attempt.KindDelegate, Timeout: s.opts.Timeout}, Instructions(t.Goal), nil)
	result := task.Result{Outcome: task.OutcomeOK, Answer: strings.TrimSpace(out.Answer), Attempt: out.Attempt.ID}
	to := task.StateDone
	if err != nil {
		result.Outcome, to = task.OutcomeError, task.StateFailed
		if errors.Is(err, task.ErrExecutionStopped) || errors.Is(err, context.Canceled) {
			result.Outcome = task.OutcomeCancelled
		}
		if result.Answer == "" {
			result.Answer = err.Error()
		}
	}
	if err := s.tasks.SetResult(id, result); err != nil {
		slog.Warn("work: record result", "task", id, "error", err)
	}
	// A stopped task has moved its epoch: this late outcome cannot revive it.
	if _, err := s.tasks.AdvanceExecution(token, to); err != nil && !errors.Is(err, task.ErrExecutionStopped) {
		slog.Warn("work: finish", "task", id, "error", err)
	}
}

// Stop cancels a task. The executor is stopped and its worktree reclaimed by
// the lifecycle; the card says stopped once the stop is confirmed.
func (s *Service) Stop(ctx context.Context, id string) (Card, error) {
	if _, ok := s.tasks.Get(id); !ok {
		return Card{}, ErrNotFound
	}
	ids, err := s.tasks.SetAside(id, task.StateCancelled)
	if err != nil {
		return Card{}, err
	}
	if s.stopper != nil {
		if err := s.stopper.Stop(ids, task.ErrExecutionStopped).Wait(ctx); err != nil {
			return Card{}, err
		}
	}
	s.publish(id)
	return s.Get(id)
}

// Observe takes a harness report for the attempt's task.
func (s *Service) Observe(record attempt.Record, p view.Progress) {
	s.mu.Lock()
	live, ok := s.live[record.TaskID]
	if ok {
		next := activityOf(p)
		if next.Line == "" {
			next.Line = live.activity.Line
		}
		live.activity = next
	}
	s.mu.Unlock()
	if ok {
		s.publish(record.TaskID)
	}
}

// Waiting marks that the task needs the person, for as long as done is not
// called: an approval or a question is open on it.
func (s *Service) Waiting(id string) (done func()) {
	s.mu.Lock()
	if live, ok := s.live[id]; ok {
		live.waiting++
	}
	s.mu.Unlock()
	s.publish(id)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if live, ok := s.live[id]; ok && live.waiting > 0 {
				live.waiting--
			}
			s.mu.Unlock()
			s.publish(id)
		})
	}
}

// ModeOf is the approval mode the task was submitted with.
func (s *Service) ModeOf(id string) string {
	var r request
	var found bool
	_ = s.book.Read(context.Background(), func(tx *ledger.ReadTx) error {
		var err error
		r, found, err = durable.Get[request](tx, requestKind, id)
		return err
	})
	if !found || r.Mode == "" {
		return ModeAuto
	}
	return r.Mode
}

// Get is the task's card.
func (s *Service) Get(id string) (Card, error) {
	t, ok := s.tasks.Get(id)
	if !ok || t.Origin != origin {
		return Card{}, ErrNotFound
	}
	return s.card(t), nil
}

// List is every task the service ran, newest first.
func (s *Service) List() []Card {
	var out []Card
	for _, t := range s.tasks.List("") {
		if t.Origin == origin {
			out = append(out, s.card(t))
		}
	}
	return out
}

func (s *Service) card(t task.Task) Card {
	c := Card{ID: t.ID, Goal: t.Goal, Agent: t.Member, Project: t.ProjectID, Thread: t.Channel, Mode: s.ModeOf(t.ID), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	s.mu.Lock()
	live, running := s.live[t.ID]
	var waiting int
	if running {
		c.Activity, waiting = live.activity, live.waiting
	}
	s.mu.Unlock()
	switch t.State {
	case task.StateDraft:
		c.State = StateQueued
	case task.StateDone:
		c.State = StateDone
	case task.StateCancelled:
		c.State = StateStopped
	case task.StateFailed:
		c.State = StateFailed
	default:
		c.State = StateRunning
		if waiting > 0 {
			c.State = StateWaiting
		}
	}
	if !running && c.State == StateRunning {
		// Left running by a process that is gone; recovery resumes or
		// fails it. Until then it is not running here.
		c.State = StateFailed
		c.Failure, c.Error = FailTransport, "interrupted: the service restarted while this task ran"
	}
	if r := t.Result; r != nil {
		switch {
		case c.State == StateDone:
			c.Answer = r.Answer
		case c.State == StateFailed && r.Outcome != task.OutcomeOK:
			c.Error = r.Answer
			c.Failure = classify(errors.New(r.Answer))
		}
	}
	return c
}

func (s *Service) publish(id string) {
	if s.feed == nil {
		return
	}
	card, err := s.Get(id)
	if err != nil {
		return
	}
	if _, err := s.feed.Publish(EventTask, card); err != nil {
		slog.Warn("work: publish", "task", id, "error", err)
	}
}

// Wait blocks until every task this process started has returned.
func (s *Service) Wait() { s.running.Wait() }

// Instructions is the executor's prompt for a goal: the work, where it
// happens, and what finishing means.
func Instructions(goal string) string {
	return strings.TrimSpace(`You are working on one task for the owner, in an isolated git worktree of the project.

Task:
` + goal + `

How to work:
- Do the task end to end. Do not stop to ask unless a decision really belongs to the owner.
- The worktree is removed when you finish. If you change code, commit it on a new branch, push it, and open a pull request; never push to the default branch.
- Before anything irreversible or visible to others — merging, tagging, releasing, deploying, deleting data, sending messages — ask for approval first.
- Finish with a short summary: what you did, links (PRs, commits), and anything left for the owner.`)
}

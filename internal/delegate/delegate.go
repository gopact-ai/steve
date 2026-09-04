// Package delegate is how one agent hands work to another.
//
// It is the only shape of agent-to-agent interaction Steve offers, and it is
// a tool call: a typed request in, a typed result out, a child task in the
// tree between them. There is no message an agent can send another agent
// outside this — not discouraged, absent — because a peer relationship with
// no hierarchy and no budget is the shape that has been shown to fail, and
// a child task is the shape that has been shown to work.
package delegate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/ctxpack"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Sessions opens the child's session wherever its agent was placed.
type Sessions interface {
	OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error)
	CloseSession(ctx context.Context, at harness.Placement, upstreamID string) error
}

// Gate mints the child's own messaging token. The messaging server
// satisfies it.
type Gate interface {
	Delegated(conversationID, agentID, taskID, delegatedBy, token, endpoint string) []capability.Extra
	Revoke(token string)
}

// Endpoints resolves where a remote child should call the messaging server.
type Endpoints interface {
	MCPEndpoint(ctx context.Context, node string) (string, error)
}

type Service struct {
	tasks      *task.Store
	roster     *roster.Roster
	sessions   Sessions
	assembler  *capability.Assembler
	workspaces project.Workspaces
	attempts   *attempt.Service
	artifacts  *artifact.Store
	gate       Gate
	endpoints  Endpoints
	node       string
	// MaxSilence is how long a child may go without a sign of life — no
	// tool call, no text — before it is cancelled. It is an idle clock,
	// not a cap: a child that builds and tests for twenty minutes while
	// reporting is left alone, one that hung is not. The child's own
	// budget (MaxElapsed) stays the hard limit.
	MaxSilence time.Duration
	// observe, when set, is told what each child is doing and how it
	// ended; the console shows it under the parent's delegate call.
	observe func(Child, view.Progress)
	// attempts_ is each child's attempt id, for the end report.
	attempts_ map[string]string
	// InlineWait is how long steve_delegate itself waits before answering
	// "running": a child that finishes in seconds comes back done in one
	// call, and a slow one does not hold the request open past any
	// client's timeout.
	InlineWait time.Duration

	mu      sync.Mutex
	pending map[string]*child
	bases   map[string]string
	spends  map[string]view.Progress
}

// child is a delegation in flight or recently finished.
type child struct {
	result  agentmcp.DelegateResult
	err     error
	started time.Time
	done    chan struct{}
}

const (
	defaultInlineWait = 20 * time.Second
	// MaxAwait bounds one steve_await call so it returns inside any MCP
	// client's request timeout; the caller calls again.
	MaxAwait = 50 * time.Second
	// keepFinished is how long a finished child's result stays retrievable
	// after it completes, for a caller that awaits late.
	keepFinished = 30 * time.Minute
)

// worktreeContract tells a delegated agent what its directory is: a
// worktree Steve snapshots when the task ends. A git init or commit
// inside it does not record anything; it only hides the files.
const worktreeContract = `

## 你的目录
- 这是 Steve 为这个子任务准备的隔离工作树；任务结束时 Steve 会快照整个目录，把改动落回项目。
- 直接写文件就行。**不要** git init / git add / git commit：目录里新建的 git 仓库会让你写的文件落不回去。
- 编译产物、下载的依赖、临时文件用完删掉，或者放到目录外；它们会跟着快照走。`

// SetLedger wires what makes a delegation an attempt: leases and the
// artifact its result becomes. Without them nothing is delegated.
func (s *Service) SetLedger(attempts *attempt.Service, artifacts *artifact.Store) {
	s.attempts = attempts
	s.artifacts = artifacts
}

func New(tasks *task.Store, r *roster.Roster, sessions Sessions, assembler *capability.Assembler, workspaces project.Workspaces, node string) *Service {
	return &Service{
		tasks: tasks, roster: r, sessions: sessions, assembler: assembler, workspaces: workspaces, node: node,
		InlineWait: defaultInlineWait, pending: map[string]*child{},
	}
}

// Start places and spawns the child, runs it detached from the caller's
// request, and answers as soon as it is running — or done, if it finished
// within InlineWait. Placement, budget, depth and cycle refusals come back
// immediately as errors: those are decided before anything runs.
func (s *Service) Start(ctx context.Context, conversationID, agentID string, req agentmcp.DelegateRequest) (agentmcp.DelegateResult, error) {
	parent, ok := s.tasks.Running(conversationID, agentID)
	if !ok {
		return agentmcp.DelegateResult{}, fmt.Errorf("%s has no running task in this conversation to delegate from", agentID)
	}
	candidate, err := s.place(ctx, agentID, req)
	if err != nil {
		return agentmcp.DelegateResult{}, err
	}
	// The child works on the parent's project in its own worktree on the
	// placed node, from the project's current canonical state. That is
	// decided before the task exists: a child that cannot get a workspace
	// is refused, not spawned.
	if s.workspaces == nil || s.attempts == nil || s.artifacts == nil {
		return agentmcp.DelegateResult{}, fmt.Errorf("delegation is not wired to workspaces and the ledger")
	}
	attemptID := attempt.NewID()
	workspace, err := s.workspaces.Materialize(ctx, project.Request{Project: parent.ProjectID, Node: candidate.Node, Isolated: true, Owner: attemptID})
	if err != nil {
		return agentmcp.DelegateResult{}, fmt.Errorf("workspace for %s: %w", candidate.Agent.ID, err)
	}
	s.rememberBase(workspace.ID, workspace.Base)
	spawned, err := s.tasks.Spawn(parent.ID, task.Task{
		Goal: goal(req.Goal), Member: candidate.Agent.ID, Node: candidate.Node,
		Origin: "delegate:" + parent.ID, ProjectID: parent.ProjectID, Workspace: workspace.Path,
		ChatID: parent.ChatID, AnchorMessage: parent.AnchorMessage, ChatType: parent.ChatType,
	})
	if err != nil {
		return agentmcp.DelegateResult{}, err
	}
	log.Printf("delegate: %s -> %s task #%s under #%s on %s", agentID, candidate.Agent.ID, spawned.ID, parent.ID, nodeLabel(candidate.Node))

	entry := &child{started: time.Now(), done: make(chan struct{})}
	entry.result = agentmcp.DelegateResult{
		TaskID: spawned.ID, Agent: candidate.Agent.ID, Node: candidate.Node, State: "running",
	}
	s.mu.Lock()
	s.pending[spawned.ID] = entry
	s.mu.Unlock()

	// Detached on purpose: the request that asked for this may be gone
	// long before the child is, and a client hanging up must not cancel
	// work that is half done on another machine.
	go s.drive(context.WithoutCancel(ctx), conversationID, agentID, parent, spawned, candidate, req, entry)

	return s.wait(ctx, entry, s.InlineWait)
}

// Await returns a child's result, waiting up to the request's bound. Only
// the caller's own descendants can be awaited: a task id is short, and one
// agent must not be able to read another's delegations.
func (s *Service) Await(ctx context.Context, conversationID, agentID string, req agentmcp.AwaitRequest) (agentmcp.DelegateResult, error) {
	caller, ok := s.tasks.Running(conversationID, agentID)
	if !ok {
		return agentmcp.DelegateResult{}, fmt.Errorf("%s has no running task in this conversation", agentID)
	}
	if !s.descends(req.TaskID, caller.ID) {
		return agentmcp.DelegateResult{}, fmt.Errorf("task %s is not a delegation of yours", req.TaskID)
	}
	wait := time.Duration(req.WaitSeconds) * time.Second
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if wait > MaxAwait {
		wait = MaxAwait
	}
	s.mu.Lock()
	entry := s.pending[req.TaskID]
	s.mu.Unlock()
	if entry == nil {
		// Not in memory — finished and forgotten, or from before a restart.
		// The task store still knows how it ended.
		return s.fromStore(req.TaskID)
	}
	return s.wait(ctx, entry, wait)
}

// Delegate is the synchronous form: start, then wait as long as it takes.
// The tool never uses it; tests and in-process callers do.
func (s *Service) Delegate(ctx context.Context, conversationID, agentID string, req agentmcp.DelegateRequest) (agentmcp.DelegateResult, error) {
	first, err := s.Start(ctx, conversationID, agentID, req)
	if err != nil {
		return first, err
	}
	if first.State != "running" {
		return s.settle(first)
	}
	s.mu.Lock()
	entry := s.pending[first.TaskID]
	s.mu.Unlock()
	if entry == nil {
		return first, nil
	}
	select {
	case <-entry.done:
	case <-ctx.Done():
		return first, ctx.Err()
	}
	return s.settle(s.snapshot(entry))
}

// settle turns a finished result into the sync form's return: the child's
// failure is the caller's error.
func (s *Service) settle(result agentmcp.DelegateResult) (agentmcp.DelegateResult, error) {
	if result.State == "failed" {
		s.mu.Lock()
		entry := s.pending[result.TaskID]
		s.mu.Unlock()
		if entry != nil && entry.err != nil {
			return result, entry.err
		}
		return result, fmt.Errorf("delegated task #%s failed", result.TaskID)
	}
	return result, nil
}

func (s *Service) wait(ctx context.Context, entry *child, wait time.Duration) (agentmcp.DelegateResult, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-entry.done:
	case <-timer.C:
	case <-ctx.Done():
		// The caller went away; the child does not. Report what we know.
	}
	return s.snapshot(entry), nil
}

func (s *Service) snapshot(entry *child) agentmcp.DelegateResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := entry.result
	out.Elapsed = time.Since(entry.started).Round(time.Second).String()
	return out
}

func (s *Service) fromStore(taskID string) (agentmcp.DelegateResult, error) {
	stored, ok := s.tasks.Get(taskID)
	if !ok {
		return agentmcp.DelegateResult{}, fmt.Errorf("no task %s", taskID)
	}
	state := "running"
	switch stored.State {
	case task.StateDone:
		state = "done"
	case task.StateFailed, task.StateCancelled:
		state = "failed"
	}
	return agentmcp.DelegateResult{
		TaskID: stored.ID, Agent: stored.Member, Node: stored.Node, State: state,
		Elapsed: stored.Budget.Elapsed.Round(time.Second).String(),
	}, nil
}

// descends reports whether taskID is under ancestorID in the tree.
func (s *Service) descends(taskID, ancestorID string) bool {
	for _, up := range s.tasks.Ancestry(taskID) {
		if up.ID == ancestorID {
			return true
		}
	}
	return false
}

// drive runs the child to completion and records how it ended, whoever is
// or is not still waiting.
func (s *Service) drive(ctx context.Context, conversationID, agentID string, parent, spawned task.Task,
	candidate roster.Candidate, req agentmcp.DelegateRequest, entry *child) {
	since := time.Now()
	var last view.Progress
	result, runErr := s.run(ctx, conversationID, agentID, parent, spawned, candidate, req, func(p view.Progress) {
		last = p
		s.report(Child{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID, Agent: candidate.Agent.ID, Node: candidate.Node,
			Goal: req.Goal, State: "running", Since: since, Elapsed: time.Since(since)}, p)
	})

	// Whatever happened, the spend is the tree's now.
	if err := s.tasks.Charge(spawned.ID); err != nil {
		log.Printf("delegate: charge task #%s to its parent: %v", spawned.ID, err)
	}
	if runErr != nil {
		result.State, result.Outcome = "failed", string(task.OutcomeError)
		if result.Answer == "" {
			result.Answer = runErr.Error()
		}
		if _, err := s.tasks.Advance(spawned.ID, task.StateFailed); err != nil {
			log.Printf("delegate: mark task #%s failed: %v", spawned.ID, err)
		}
		runErr = fmt.Errorf("delegated task #%s on %s failed: %w", spawned.ID, candidate.Agent.ID, runErr)
	} else {
		result.State = "done"
		if _, err := s.tasks.Advance(spawned.ID, task.StateDone); err != nil {
			log.Printf("delegate: mark task #%s done: %v", spawned.ID, err)
		}
	}
	result.TaskID, result.Agent, result.Node = spawned.ID, candidate.Agent.ID, candidate.Node

	s.mu.Lock()
	entry.result, entry.err = result, runErr
	s.mu.Unlock()
	close(entry.done)
	log.Printf("delegate: task #%s %s on %s", spawned.ID, result.State, nodeLabel(candidate.Node))
	s.report(Child{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID, Agent: candidate.Agent.ID, Node: candidate.Node,
		Goal: req.Goal, State: result.State, Since: since, Elapsed: time.Since(since), Answer: result.Answer, Refs: result.Refs, Attempt: s.attemptOf(spawned.ID)}, last)

	// Keep the result around for a late awaiter, then let it go.
	time.AfterFunc(keepFinished, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pending[spawned.ID] == entry {
			delete(s.pending, spawned.ID)
		}
	})
}

// Child is a delegated task as an observer sees it.
type Child struct {
	Conversation, ParentTask, Task string
	Agent, Node, Goal              string
	State                          string // running | done | failed
	Since                          time.Time
	Elapsed                        time.Duration
	Answer                         string
	Refs                           []string
	// Attempt is the child's attempt, once opened: its record holds the
	// before and after snapshots.
	Attempt string
}

// SetObserver installs where a child's progress goes; nil discards it.
func (s *Service) SetObserver(observe func(Child, view.Progress)) { s.observe = observe }

// attemptOf is the attempt a child ran as, remembered when it opened.
func (s *Service) attemptOf(childID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts_[childID]
}

func (s *Service) rememberAttempt(childID, attemptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts_ == nil {
		s.attempts_ = map[string]string{}
	}
	s.attempts_[childID] = attemptID
}

func (s *Service) report(c Child, p view.Progress) {
	if s.observe != nil {
		s.observe(c, p)
	}
}

func (s *Service) SetGate(g Gate)           { s.gate = g }
func (s *Service) SetEndpoints(e Endpoints) { s.endpoints = e }

func (s *Service) run(ctx context.Context, conversationID, delegatedBy string, parent, child task.Task,
	candidate roster.Candidate, req agentmcp.DelegateRequest, progress func(view.Progress)) (agentmcp.DelegateResult, error) {
	result := agentmcp.DelegateResult{TaskID: child.ID, Agent: candidate.Agent.ID, Node: candidate.Node}

	// The child's own token: bound to the child task, revoked when it ends.
	var extras []capability.Extra
	if s.gate != nil {
		token, err := newToken()
		if err != nil {
			return result, err
		}
		endpoint := ""
		if candidate.Node != "" && s.endpoints != nil {
			endpoint, err = s.endpoints.MCPEndpoint(ctx, candidate.Node)
			if err != nil {
				// Losing milestone cards is a degradation; losing the
				// delegation is not. The child runs without the send
				// primitive.
				log.Printf("delegate: node %q messaging endpoint: %v", candidate.Node, err)
				endpoint = ""
			}
		}
		if candidate.Node == "" || endpoint != "" {
			extras = s.gate.Delegated(conversationID, candidate.Agent.ID, child.ID, delegatedBy, token, endpoint)
			defer s.gate.Revoke(token)
		}
	}

	caps, err := s.assembler.AssembleExtra(candidate.Agent, home.ModeGuest, extras)
	if err != nil {
		return result, fmt.Errorf("assemble capabilities for %s: %w", candidate.Agent.ID, err)
	}

	refs := parseRefArgs(req.Refs)
	now := time.Now()
	payload, err := ctxpack.Build(ctxpack.Context{
		Goal:      delegateBrief(candidate) + req.Goal + expectLine(req.Expect),
		Ancestry:  ancestry(s.tasks, child),
		Refs:      refs,
		Bearings:  ctxpack.Bearings(child.Workspace, refs),
		Facts:     candidate.Capabilities,
		TurnsLeft: child.Budget.MaxTurns,
		Deadline:  child.Deadline(now).Format("15:04"),
	})
	if err != nil {
		return result, err
	}

	ctx, cancel := context.WithTimeout(ctx, child.Budget.MaxElapsed)
	defer cancel()
	touch := func() {}
	if s.MaxSilence > 0 {
		var stop func()
		ctx, stop, touch = idle.WithTimeout(ctx, s.MaxSilence)
		defer stop()
	}

	at := harness.Placement{Node: candidate.Node, Harness: candidate.Harness}
	if _, err := s.tasks.Begin(child.ID, candidate.Agent.ID, orHub(candidate.Node, s.node), ""); err != nil {
		return result, err
	}
	workspace := project.Workspace{ID: s.worktreeID(child), Project: parent.ProjectID, Node: candidate.Node, Path: child.Workspace, Kind: project.KindWorktree}
	base := s.baseOf(workspace.ID)
	record, err := s.attempts.Open(ctx, attempt.Spec{
		ID: strings.TrimPrefix(workspace.ID, "wt-"), TaskID: child.ID, TurnID: "delegate/" + child.ID, Kind: attempt.KindDelegate,
		Project: parent.ProjectID, Node: candidate.Node, Harness: candidate.Harness, Agent: candidate.Agent.ID, Slots: candidate.Slots,
		Region: candidate.Region, CanonicalRegion: s.homeRegion(ctx, parent.ProjectID),
		Workspace: workspace, Scope: attempt.ScopePathSet, Base: base, By: delegatedBy, Requires: req.Requires,
	})
	if err != nil {
		s.finish(child.ID, task.OutcomeError)
		_ = s.artifacts.Discard(context.WithoutCancel(ctx), workspace)
		return result, fmt.Errorf("lease delegation: %w", err)
	}
	s.rememberAttempt(child.ID, record.ID)
	beat, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	lost := s.attempts.Heartbeat(beat, record.ID)
	failAttempt := func(cause error) {
		_, _ = s.attempts.Fail(context.WithoutCancel(ctx), record.ID, "delegate", cause.Error())
		_ = s.artifacts.Discard(context.WithoutCancel(ctx), workspace)
	}
	// The machine's final word on the requirement, taken now, before a
	// session is opened there.
	admission, bindings, err := s.roster.Admit(ctx, candidate, req.Requires, candidate.Agent.MCPServers, record.ID)
	if err != nil {
		s.finish(child.ID, task.OutcomeError)
		failAttempt(err)
		return result, fmt.Errorf("admission on %s: %w", at, err)
	}
	if admission.Refused() {
		err := &Refusal{Code: "REFUSED_AT_ADMISSION", Retryable: true, Requires: req.Requires,
			Failures: []Failure{{Agent: candidate.Agent.ID, Node: candidate.Node, Reasons: admission.Atoms}}}
		s.finish(child.ID, task.OutcomeError)
		failAttempt(err)
		return result, err
	}
	if len(bindings) > 0 {
		defer s.roster.Release(context.WithoutCancel(ctx), candidate.Node, record.ID)
	}
	session, err := s.sessions.OpenSession(ctx, at, "", child.Workspace, append(caps.MCPServers, roster.ToMCP(bindings)...))
	if err != nil {
		s.finish(child.ID, task.OutcomeError)
		failAttempt(err)
		return result, fmt.Errorf("open session on %s: %w", at, err)
	}
	harness.ApplyPreferences(ctx, session, candidate.Agent.ID, candidate.Agent.Model, candidate.Agent.Options)
	_, _ = s.attempts.Advance(ctx, record.ID, attempt.Prepared, "delegate", func(r *attempt.Record) { r.Admission = &admission })
	_, _ = s.attempts.Advance(ctx, record.ID, attempt.Running, "delegate", nil)
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	go func() {
		select {
		case <-lost:
			log.Printf("delegate: attempt %s lost its lease; cancelling task #%s", record.ID, child.ID)
			stopRun()
		case <-runCtx.Done():
		}
	}()
	ctx = runCtx
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer closeCancel()
		if err := s.sessions.CloseSession(closeCtx, at, session.ID()); err != nil {
			session.Abort()
		}
	}()

	prompt := payload.Render() + exec.ReportingContract + worktreeContract
	if caps.Instructions != "" {
		prompt = caps.Instructions + "\n\n" + prompt
	}
	var last view.Progress
	answer, _, err := session.Prompt(ctx, prompt, func(p view.Progress) {
		last = p
		touch()
		if progress != nil {
			progress(p)
		}
	})
	s.spent(child.ID, last)
	if err != nil {
		s.finish(child.ID, outcomeOf(err))
		failAttempt(err)
		return result, err
	}
	// What the child said is the result from here on, whatever happens
	// to its files: every return below carries it.
	result.Outcome = string(task.OutcomeOK)
	result.Answer = strings.TrimSpace(answer)
	for _, ref := range exec.ParseRefs(answer) {
		result.Refs = append(result.Refs, ref.Kind+" "+ref.Value)
	}
	// The child's result becomes an artifact bound to its name and queued
	// to land once the parent's turn releases the canonical lock.
	published, changed, err := s.artifacts.Publish(ctx, workspace, base, record.ID, "delegation #"+child.ID)
	if err != nil {
		s.finish(child.ID, task.OutcomeError)
		failAttempt(err)
		return result, fmt.Errorf("publish delegation result: %w", err)
	}
	for _, to := range []attempt.State{attempt.Snapshotted, attempt.Published, attempt.Durable, attempt.BindReady} {
		if _, err := s.attempts.Advance(ctx, record.ID, to, "delegate", func(r *attempt.Record) {
			if to == attempt.BindReady {
				r.Result = &attempt.Result{Artifact: published.ID}
			}
		}); err != nil {
			s.finish(child.ID, task.OutcomeError)
			failAttempt(err)
			return result, err
		}
	}
	name := "steve/" + child.ID + "/result"
	current, _, _ := s.artifacts.Resolve(ctx, name)
	if _, err := s.artifacts.Bind(ctx, name, current.Version, published.ID); err != nil {
		_, _ = s.attempts.Advance(context.WithoutCancel(ctx), record.ID, attempt.BindConflict, "delegate", nil)
		s.finish(child.ID, task.OutcomeError)
		return result, err
	}
	_, _ = s.attempts.Advance(ctx, record.ID, attempt.Bound, "delegate", nil)
	_ = s.artifacts.Discard(context.WithoutCancel(ctx), workspace)
	if changed {
		result.Refs = append(result.Refs, "artifact "+published.ID)
		// The parent asked for this and holds the canonical lock right now:
		// the result lands under its lease, into the directory it is
		// working in, so the parent sees the files this turn. A conflict
		// is queued for after the turn and reported as such.
		if lease, ok := s.parentLease(ctx, parent); ok {
			if p, found, perr := s.artifacts.Project(ctx, parent.ProjectID); perr == nil && found {
				landed, lerr := s.artifacts.LandUnder(ctx, p, published.ID, "task #"+child.ID, lease)
				switch {
				case lerr == nil:
					result.Refs = append(result.Refs, fmt.Sprintf("landed into your working directory: %d path(s)", len(landed.Paths)))
					s.finish(child.ID, task.OutcomeOK)
					return result, nil
				default:
					log.Printf("delegate: land %s under parent's lease: %v", published.ID, lerr)
					result.Refs = append(result.Refs, "not landed yet: "+lerr.Error())
				}
			}
		}
		if err := s.artifacts.Defer(ctx, parent.ProjectID, published.ID, "task #"+child.ID); err != nil {
			log.Printf("delegate: queue landing of %s: %v", published.ID, err)
		}
		result.Refs = append(result.Refs, "queued to land when this turn ends")
	}
	s.finish(child.ID, task.OutcomeOK)
	return result, nil
}

// place picks who does the work. The caller is excluded from its own
// delegation: handing work to yourself is a loop with extra steps.
func (s *Service) place(ctx context.Context, caller string, req agentmcp.DelegateRequest) (roster.Candidate, error) {
	if req.Agent != "" {
		if req.Agent == caller {
			return roster.Candidate{}, fmt.Errorf("%s cannot delegate to itself", caller)
		}
		for _, c := range s.roster.All(ctx) {
			if c.Agent.ID != req.Agent {
				continue
			}
			if !c.Eligible {
				return roster.Candidate{}, &Refusal{Code: "NOT_ELIGIBLE", Retryable: true, Requires: req.Requires,
					Failures: []Failure{{Agent: c.Agent.ID, Node: c.Node, Why: c.Why}}}
			}
			// A named agent still has to meet the requirements.
			compiled, err := ability.Compile(req.Requires)
			if err != nil {
				return roster.Candidate{}, &Refusal{Code: "BAD_REQUIREMENT", Requires: req.Requires, Why: err.Error()}
			}
			if m := c.Match(compiled); !m.OK() {
				return roster.Candidate{}, &Refusal{Code: "UNMET", Requires: req.Requires,
					Failures: []Failure{{Agent: c.Agent.ID, Node: c.Node, Reasons: m.Atoms}}}
			}
			return c, nil
		}
		return roster.Candidate{}, &Refusal{Code: "UNKNOWN_AGENT", Requires: req.Requires, Why: "no agent named " + req.Agent}
	}
	candidates := s.roster.Candidates(ctx, req.Requires, []string{caller})
	if len(candidates) == 0 {
		return roster.Candidate{}, s.refusal(ctx, caller, req.Requires)
	}
	return candidates[0], nil
}

// Refusal is a delegation that could not be placed, in a form an agent
// can act on without parsing prose: a code, whether waiting might help,
// and per candidate which atoms of the requirement failed. It is also
// readable, since its text is the JSON.
type Refusal struct {
	Code      string    `json:"code"`
	Retryable bool      `json:"retryable"`
	Requires  []string  `json:"requires,omitempty"`
	Why       string    `json:"why,omitempty"`
	Failures  []Failure `json:"failures,omitempty"`
}

// Failure is one candidate that was not chosen and why.
type Failure struct {
	Agent   string               `json:"agent"`
	Node    string               `json:"node,omitempty"`
	Why     string               `json:"why,omitempty"`
	Reasons []ability.AtomResult `json:"reasons,omitempty"`
}

func (r *Refusal) Error() string {
	body, err := json.Marshal(r)
	if err != nil {
		return "delegation refused: " + r.Code
	}
	return "delegation refused: " + string(body)
}

// refusal explains an empty candidate list per agent: what each one was
// missing, or why it could not take work at all.
func (s *Service) refusal(ctx context.Context, caller string, requires []string) *Refusal {
	compiled, err := ability.Compile(requires)
	if err != nil {
		return &Refusal{Code: "BAD_REQUIREMENT", Requires: requires, Why: err.Error()}
	}
	out := &Refusal{Code: "NO_CANDIDATE", Requires: requires}
	for _, c := range s.roster.All(ctx) {
		if c.Agent.ID == caller {
			continue
		}
		f := Failure{Agent: c.Agent.ID, Node: c.Node}
		if !c.Eligible {
			f.Why = c.Why
			out.Retryable = true
		}
		if m := c.Match(compiled); !m.OK() {
			for _, atom := range m.Atoms {
				if atom.Verdict != ability.True {
					f.Reasons = append(f.Reasons, atom)
				}
			}
		}
		out.Failures = append(out.Failures, f)
	}
	return out
}

func (s *Service) finish(id string, outcome task.Outcome) {
	s.mu.Lock()
	spent := s.spends[id]
	delete(s.spends, id)
	s.mu.Unlock()
	tokens := task.FromUsage(spent.Usage.InputTokens, spent.Usage.OutputTokens, spent.Usage.CacheReadTokens, spent.Usage.CacheWriteTokens)
	if _, err := s.tasks.FinishAs(id, outcome, tokens, 0, spent.Settings.Model); err != nil {
		log.Printf("delegate: finish task #%s: %v", id, err)
	}
}

// spent remembers a child's last progress until its task is finished.
func (s *Service) spent(id string, p view.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spends == nil {
		s.spends = map[string]view.Progress{}
	}
	s.spends[id] = p
}

// ancestry is the chain of goals above the child, root first — what its
// work serves, never the transcripts of those tasks.
func ancestry(tasks *task.Store, child task.Task) []string {
	up := tasks.Ancestry(child.ID)
	out := make([]string, 0, len(up))
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i].Goal)
	}
	return out
}

// parseRefArgs reads "git abc123" / "blob sha256:…" into typed refs. Text
// that is not a ref is dropped rather than smuggled in as content.
func parseRefArgs(args []string) []plan.Ref {
	var out []plan.Ref
	for _, raw := range args {
		kind, value, ok := strings.Cut(strings.TrimSpace(raw), " ")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		switch kind {
		case "git", "blob", "task":
			out = append(out, plan.Ref{Kind: kind, Value: strings.TrimSpace(value)})
		}
	}
	return out
}

func expectLine(expect string) string {
	if strings.TrimSpace(expect) == "" {
		return ""
	}
	return "\n\n完成的标准：" + strings.TrimSpace(expect)
}

func outcomeOf(err error) task.Outcome {
	switch {
	case err == nil:
		return task.OutcomeOK
	case ctxErr(err, context.DeadlineExceeded):
		return task.OutcomeTimeout
	case ctxErr(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

func ctxErr(err, target error) bool {
	return err == target || strings.Contains(err.Error(), target.Error())
}

const goalLimit = 120

func goal(text string) string {
	trimmed := strings.TrimSpace(text)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
	if len([]rune(trimmed)) <= goalLimit {
		return trimmed
	}
	return string([]rune(trimmed)[:goalLimit]) + "…"
}

func newToken() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// parentLease finds the canonical lease the parent's in-place attempt holds.
func (s *Service) parentLease(ctx context.Context, parent task.Task) (ledger.Lease, bool) {
	records, err := s.attempts.ForTask(ctx, parent.ID)
	if err != nil {
		return ledger.Lease{}, false
	}
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.State.Terminal() || r.Scope != attempt.ScopeUnrestricted {
			continue
		}
		for _, lease := range r.Leases {
			if lease.Key == "canonical:"+parent.ProjectID {
				return lease, true
			}
		}
	}
	return ledger.Lease{}, false
}

// rememberBase keeps the base a child's worktree came from until run()
// needs it, keyed by the worktree id.
func (s *Service) rememberBase(attemptID, base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bases == nil {
		s.bases = map[string]string{}
	}
	s.bases[attemptID] = base
}

func (s *Service) baseOf(workspaceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.bases[workspaceID]
	delete(s.bases, workspaceID)
	return base
}

// delegateBrief tells the child what it is: the agent that was chosen for
// this, on the machine that can do it. Without it a child that reads
// "on a machine with X" in its goal may conclude it should delegate again.
func delegateBrief(c roster.Candidate) string {
	return fmt.Sprintf("You are the delegate for this work, running on %s (capabilities: %s). Do it here, in this directory, yourself; delegating further is not available to you.\n\n",
		nodeLabel(c.Node), strings.Join(c.Capabilities, ", "))
}

// worktreeID recovers the workspace id from the child's directory: the
// materializer names the directory after it.
func (s *Service) worktreeID(child task.Task) string {
	return filepath.Base(child.Workspace)
}

func nodeLabel(node string) string { return nodewire.Place(node) }

func orHub(node, hub string) string {
	if node == "" {
		return hub
	}
	return node
}

// homeRegion is the region of the project's canonical workspace.
func (s *Service) homeRegion(ctx context.Context, projectID string) string {
	if s.artifacts == nil || s.roster == nil {
		return ""
	}
	p, ok, err := s.artifacts.Project(ctx, projectID)
	if err != nil || !ok {
		return ""
	}
	return s.roster.RegionOf(p.Home.Node)
}

// Fleet renders the roster for an agent: one line per other agent, with
// its machine, harness, observed model, whether it can take work now, and
// its manifest in selector form. Requirements are written in that form.
func (s *Service) Fleet(ctx context.Context, _ string, caller string, requires []string) (string, error) {
	var b strings.Builder
	b.WriteString("Agents and what their machines can do. Write requires as kind:id (tool:docker, mcp:github, hardware:gpu, model:claude*, network:internal); a bare word is a tag.\n")
	compiled, err := ability.Compile(requires)
	if err != nil {
		return "", err
	}
	if !compiled.Empty() {
		fmt.Fprintf(&b, "Judged against %v.\n", requires)
	}
	for _, c := range s.roster.All(ctx) {
		if c.Agent.ID == caller {
			continue
		}
		state := "ready"
		if !c.Eligible {
			state = "blocked: " + c.Why
		}
		if !compiled.Empty() {
			if m := c.Match(compiled); m.OK() {
				state += "; meets the requirement"
			} else {
				state += "; lacks " + m.Unmet()
			}
		}
		model := c.Model
		if model == "" {
			model = "model unknown"
		}
		about := ""
		if c.Agent.About != "" {
			about = "\n  good for: " + c.Agent.About
		}
		fmt.Fprintf(&b, "- %s on %s (%s, %s) — %s%s\n  %s\n", c.Agent.ID, nodewire.Place(c.Node), c.Harness, model, state, about, ability.Compact(c.Snapshot, c.Harness, 10))
	}
	return b.String(), nil
}

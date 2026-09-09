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
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log/slog"
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
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/text"
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
	executions *execution.Registry
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
	MaxSilence   time.Duration
	RegisterIdle idle.Registrar
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

	// deliver carries a finished child's result into its parent's
	// conversation; deliverMu serialises deliveries per process so a
	// turn ending and a child ending at once send one message, not two.
	spawnGuard func(context.Context, *ledger.Tx) error
	deliver    func(context.Context, Delivery) error
	deliverMu  sync.Mutex

	mu                 sync.Mutex
	pending            map[string]*child
	bases              map[string]string
	spends             map[string]view.Progress
	recoveryQuestions  map[string]*recoveryNotice
	recoveryQuestion   func(context.Context, RecoveryQuestion) (view.Answer, error)
	retainedPermission func(context.Context, QuestionBinding, permission.Ask) (acp.RequestPermissionOutcome, error)
	retainedQuestion   func(context.Context, QuestionBinding, view.Question) (view.Answer, error)
}

// child is a delegation in flight or recently finished.
type child struct {
	waiters int
	scope   *execution.Scope
	result  agentmcp.DelegateResult
	err     error
	started time.Time
	done    chan struct{}
	session string
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

// deadlineText is when the child must be done, or nothing when its
// budget has no end.
func deadlineText(child task.Task, now time.Time) string {
	if child.Budget.MaxElapsed <= 0 {
		return ""
	}
	return child.Deadline(now).Format("15:04")
}

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
func (s *Service) SetExecution(r *execution.Registry) { s.executions = r }

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
	return s.start(ctx, conversationID, agentID, req, s.InlineWait)
}

func (s *Service) start(ctx context.Context, conversationID, agentID string, req agentmcp.DelegateRequest, inlineWait time.Duration) (agentmcp.DelegateResult, error) {
	requestCtx := ctx
	parent, accepted, err := s.parentTask(ctx, conversationID, agentID)
	if err != nil {
		return agentmcp.DelegateResult{}, err
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
	if s.executions != nil {
		preparation, err := s.executions.BeginAccepted(s.executions.Detached(ctx), execution.Key{TaskID: parent.ID, InstanceID: "spawn/" + attemptID}, accepted)
		if err != nil {
			return agentmcp.DelegateResult{}, err
		}
		defer preparation.Finish(nil)
		ctx = preparation.Context()
	}
	workspace, err := s.workspaces.Materialize(ctx, project.Request{Project: parent.ProjectID, Node: candidate.Node, Isolated: true, Owner: attemptID})
	if err != nil {
		return agentmcp.DelegateResult{}, fmt.Errorf("workspace for %s: %w", candidate.Agent.ID, err)
	}
	if err := ctx.Err(); err != nil {
		s.discardUnused(ctx, workspace, parent)
		return agentmcp.DelegateResult{}, err
	}
	s.rememberBase(workspace.ID, workspace.Base)
	childSpec := task.Task{
		Goal: goal(req.Goal), Member: candidate.Agent.ID, Node: candidate.Node,
		Origin: "delegate:" + parent.ID, ProjectID: parent.ProjectID, Workspace: workspace.Path,
		ChatID: parent.ChatID, AnchorMessage: parent.AnchorMessage, ChatType: parent.ChatType,
	}
	var spawned task.Task
	if accepted != nil {
		guard, guardErr := s.fixedGuard(requestCtx)
		if guardErr != nil {
			err = guardErr
		} else {
			spawned, err = s.tasks.SpawnAuthorized(ctx, *accepted, childSpec, guard)
		}
	} else {
		spawned, err = s.tasks.Spawn(parent.ID, childSpec)
	}
	if err != nil {
		s.discardUnused(ctx, workspace, parent)
		return agentmcp.DelegateResult{}, err
	}
	slog.Info(fmt.Sprintf("delegate: %s -> %s task #%s under #%s on %s", agentID, candidate.Agent.ID, spawned.ID, parent.ID, nodeLabel(candidate.Node)),
		"task", spawned.ID, "parent", parent.ID, "attempt", ledgerAttemptID(workspace), "conversation", conversationID, "agent", candidate.Agent.ID, "node", candidate.Node)

	var scope *execution.Scope
	if s.executions != nil {
		var err error
		scope, err = s.executions.Begin(s.executions.Detached(ctx), execution.Key{TaskID: spawned.ID, InstanceID: "delegate/" + spawned.ID, AttemptID: attemptID})
		if err != nil {
			s.discardUnused(ctx, workspace, parent)
			return agentmcp.DelegateResult{}, err
		}
	}
	entry := &child{scope: scope, started: time.Now(), done: make(chan struct{}), waiters: 1}
	entry.result = agentmcp.DelegateResult{
		TaskID: spawned.ID, Agent: candidate.Agent.ID, Node: candidate.Node, State: task.StateRunning,
	}
	s.mu.Lock()
	s.pending[spawned.ID] = entry
	s.mu.Unlock()
	// Register the child before Start can return: opening its session may
	// take longer than the parent's remaining turn, even without progress.
	s.report(Child{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID,
		Agent: candidate.Agent.ID, Node: candidate.Node, Goal: req.Goal, State: task.StateRunning, Since: entry.started}, view.Progress{})

	// Detached on purpose: the request that asked for this may be gone
	// long before the child is, and a client hanging up must not cancel
	// work that is half done on another machine.
	driveCtx := context.WithoutCancel(ctx)
	if scope != nil {
		driveCtx = scope.Context()
	}
	go s.drive(driveCtx, conversationID, agentID, parent, spawned, candidate, req, entry)

	first, err := s.waitRegistered(requestCtx, entry, inlineWait)
	if err == nil && first.State == task.StateRunning && s.deliver != nil {
		first.Note = "Still running. You need not wait: when it ends, Steve sends its result into this conversation as a new message. End your turn if nothing else is left."
	}
	return first, err
}

// Await returns a child's result, waiting up to the request's bound. Only
// the caller's own descendants can be awaited: a task id is short, and one
// agent must not be able to read another's delegations.
func (s *Service) Await(ctx context.Context, conversationID, agentID string, req agentmcp.AwaitRequest) (agentmcp.DelegateResult, error) {
	caller, _, err := s.parentTask(ctx, conversationID, agentID)
	if err != nil {
		return agentmcp.DelegateResult{}, err
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
		return s.fromStore(ctx, req.TaskID)
	}
	return s.wait(ctx, entry, wait)
}

// Delegate is the synchronous form: start, then wait as long as it takes.
// The tool never uses it; tests and in-process callers do.
func (s *Service) Delegate(ctx context.Context, conversationID, agentID string, req agentmcp.DelegateRequest) (agentmcp.DelegateResult, error) {
	first, err := s.start(ctx, conversationID, agentID, req, -1)
	if err != nil {
		return first, err
	}
	if first.State != task.StateRunning {
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
	s.collect(result.TaskID, result)
	if result.State == task.StateFailed {
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
	s.mu.Lock()
	entry.waiters++
	s.mu.Unlock()
	return s.waitRegistered(ctx, entry, wait)
}

func (s *Service) waitRegistered(ctx context.Context, entry *child, wait time.Duration) (agentmcp.DelegateResult, error) {
	defer func() {
		s.mu.Lock()
		entry.waiters--
		id, done := entry.result.TaskID, entry.result.State == task.StateDone || entry.result.State == task.StateFailed
		s.mu.Unlock()
		if done {
			if tracked, ok := s.tasks.Get(id); ok {
				s.flushIfIdle(ctx, tracked.Parent)
			}
		}
	}()
	var deadline <-chan time.Time
	if wait >= 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-entry.done:
	case <-deadline:
	case <-ctx.Done():
		// The caller went away; the child does not. Report what we know.
	}
	out := s.snapshot(entry)
	if ctx.Err() == nil {
		if err := s.collectContext(ctx, out.TaskID, out); err != nil {
			return agentmcp.DelegateResult{}, err
		}
	}
	return out, nil
}

func (s *Service) snapshot(entry *child) agentmcp.DelegateResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := entry.result
	out.Elapsed = time.Since(entry.started).Round(time.Second).String()
	return out
}

func (s *Service) fromStore(ctx context.Context, taskID string) (agentmcp.DelegateResult, error) {
	stored, ok := s.tasks.Get(taskID)
	if ok && stored.Result != nil && stored.Finished() && (len(stored.Attempts) == 0 || !stored.Attempts[len(stored.Attempts)-1].Open()) {
		out := agentmcp.DelegateResult{TaskID: stored.ID, Agent: stored.Member, Node: stored.Node, State: task.StateDone,
			Elapsed: stored.UpdatedAt.Sub(stored.CreatedAt).Round(time.Second).String(),
			Outcome: stored.Result.Outcome, Answer: stored.Result.Answer, Refs: append([]string(nil), stored.Result.Refs...)}
		if stored.State != task.StateDone {
			out.State = task.StateFailed
		}
		if err := s.collectContext(ctx, taskID, out); err != nil {
			return agentmcp.DelegateResult{}, err
		}
		return out, nil
	}
	if !ok {
		return agentmcp.DelegateResult{}, fmt.Errorf("no task %s", taskID)
	}
	if stored.Delegated() && stored.Result == nil && stored.State != task.StateCancelled {
		return agentmcp.DelegateResult{TaskID: stored.ID, Agent: stored.Member, Node: stored.Node, State: task.StateRunning}, nil
	}

	state := task.StateRunning
	switch stored.State {
	case task.StateDone:
		state = task.StateDone
	case task.StateFailed, task.StateCancelled:
		state = task.StateFailed
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
	since := entry.started
	var last view.Progress
	result, runErr := s.run(ctx, conversationID, agentID, parent, spawned, candidate, req, func(p view.Progress) {
		last = p
		s.report(Child{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID, Agent: candidate.Agent.ID, Node: candidate.Node,
			Goal: req.Goal, State: task.StateRunning, Since: since, Elapsed: time.Since(since)}, p)
	})

	s.completeChild(ctx, conversationID, parent, spawned, req.Goal, entry, result, runErr, last)
}

func (s *Service) completeChild(ctx context.Context, conversationID string, parent, spawned task.Task, description string, entry *child, result agentmcp.DelegateResult, runErr error, last view.Progress) {
	since := entry.started

	var detached *execution.RetainedObserverDetached
	if errors.As(runErr, &detached) {
		s.detachChild(spawned, entry, detached)
		return
	}
	s.mu.Lock()
	managedSession := entry.session
	s.mu.Unlock()
	if strings.HasPrefix(managedSession, "ns_") && s.canSettleStopped(ctx, runErr) {
		var cancel context.CancelFunc
		ctx, cancel = lifecycle.Cleanup(ctx)
		defer cancel()
	}
	var retainedRecord attempt.Record
	if strings.HasPrefix(managedSession, "ns_") {
		var err error
		retainedRecord, err = s.attempts.Get(ctx, s.attemptOf(spawned.ID))
		if err == nil && !retainedRecord.State.Terminal() {
			err = errors.New("delegate result has not been durably settled")
		}
		if err == nil {
			err = s.finishFromRecord(retainedRecord, outcomeOf(runErr))
		}
		if err != nil {
			binding := QuestionBinding{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID, Attempt: s.attemptOf(spawned.ID), Node: spawned.Node, Agent: spawned.Member, Project: spawned.ProjectID, Session: managedSession}
			s.reportRecovery(ctx, binding, "task-bookkeeping", "核对已提交的子任务结果和预算", "原执行结果尚未完整写入任务记录。", "预算或任务状态的持久化失败，不能提前宣布完成。", "建议恢复存储后核对同一次执行。")
			s.detachChild(spawned, entry, &execution.RetainedObserverDetached{AttemptID: binding.Attempt, NodeID: binding.Node, SessionID: binding.Session, Cause: err})
			return
		}
	}

	if entry.scope != nil {
		defer func() {
			var unresolved error
			if errors.Is(runErr, harness.ErrStopUnconfirmed) {
				unresolved = runErr
			}
			entry.scope.Finish(unresolved)
		}()
	}
	current, _ := s.tasks.Get(spawned.ID)
	if current.State == task.StatePaused || current.State == task.StateCancelled {
		result.State, result.Outcome = current.State, task.OutcomeCancelled
	} else if runErr != nil {
		result.State, result.Outcome = task.StateFailed, task.OutcomeError
		if result.Answer == "" {
			result.Answer = runErr.Error()
		}
		if _, err := s.advanceExecution(ctx, spawned.ID, task.StateFailed); err != nil {
			slog.Error(fmt.Sprintf("delegate: mark task #%s failed: %v", spawned.ID, err), "task", spawned.ID, "parent", parent.ID, "attempt", s.attemptOf(spawned.ID), "conversation", conversationID, "node", spawned.Node)
			if strings.HasPrefix(managedSession, "ns_") {
				s.reportRecovery(ctx, questionBinding(parent, spawned, retainedRecord), "task-state", "保存子任务的已提交执行状态", "任务状态尚未完整保存。", "已有执行结果保持可恢复，不能提前报告任务结束。", "建议恢复存储后核对同一次执行。")
				s.detachChild(spawned, entry, &execution.RetainedObserverDetached{AttemptID: retainedRecord.ID, NodeID: retainedRecord.Node, SessionID: managedSession, Cause: err})
				return
			}
		}
		runErr = fmt.Errorf("delegated task #%s on %s failed: %w", spawned.ID, spawned.Member, runErr)
	} else {
		result.State = task.StateDone
		if _, err := s.advanceExecution(ctx, spawned.ID, task.StateDone); err != nil {
			slog.Error(fmt.Sprintf("delegate: mark task #%s done: %v", spawned.ID, err), "task", spawned.ID, "parent", parent.ID, "attempt", s.attemptOf(spawned.ID), "conversation", conversationID, "node", spawned.Node)
			if strings.HasPrefix(managedSession, "ns_") {
				s.reportRecovery(ctx, questionBinding(parent, spawned, retainedRecord), "task-state", "保存子任务的已提交执行状态", "任务状态尚未完整保存。", "已有执行结果保持可恢复，不能提前报告任务结束。", "建议恢复存储后核对同一次执行。")
				s.detachChild(spawned, entry, &execution.RetainedObserverDetached{AttemptID: retainedRecord.ID, NodeID: retainedRecord.Node, SessionID: managedSession, Cause: err})
				return
			}
		}
	}
	if latest, ok := s.tasks.Get(spawned.ID); ok && (latest.State == task.StatePaused || latest.State == task.StateCancelled) {
		result.State, result.Outcome = latest.State, task.OutcomeCancelled
	}
	result.TaskID, result.Agent, result.Node = spawned.ID, spawned.Member, spawned.Node

	if err := s.tasks.SetResult(spawned.ID, task.Result{Outcome: result.Outcome, Answer: result.Answer, Refs: result.Refs, Attempt: s.attemptOf(spawned.ID)}); err != nil {
		slog.Error(fmt.Sprintf("delegate: record result of task #%s: %v", spawned.ID, err), "task", spawned.ID, "parent", parent.ID, "attempt", s.attemptOf(spawned.ID), "conversation", conversationID, "node", spawned.Node)
		if strings.HasPrefix(managedSession, "ns_") {
			s.reportRecovery(ctx, questionBinding(parent, spawned, retainedRecord), "task-result", "保存原执行的完整答复到子任务记录", "答复尚未完整写入任务。", "已提交的执行结果仍保留，不能提前向父任务宣布完成。", "建议恢复存储后重新核对。")
			s.detachChild(spawned, entry, &execution.RetainedObserverDetached{AttemptID: retainedRecord.ID, NodeID: retainedRecord.Node, SessionID: managedSession, Cause: err})
			return
		}
	}
	s.mu.Lock()
	entry.result, entry.err = result, runErr
	s.mu.Unlock()
	close(entry.done)
	slog.Info(fmt.Sprintf("delegate: task #%s %s on %s", spawned.ID, result.State, nodeLabel(spawned.Node)),
		"task", spawned.ID, "parent", parent.ID, "attempt", s.attemptOf(spawned.ID), "conversation", conversationID, "agent", spawned.Member, "node", spawned.Node)
	s.report(Child{Conversation: conversationID, ParentTask: parent.ID, Task: spawned.ID, Agent: spawned.Member, Node: spawned.Node,
		Goal: description, State: result.State, Since: since, Elapsed: time.Since(since), Answer: result.Answer, Refs: result.Refs, Attempt: s.attemptOf(spawned.ID)}, last)

	// The parent is told now if no turn of it is running; a running turn
	// is told when it ends. An awaiter that already read the result in
	// this turn marked it collected, and nothing more is sent.
	s.flushIfIdle(ctx, parent.ID)

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
	State                          task.State // running | done | failed, or a paused/cancelled task
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
	if entry := s.pending[childID]; entry != nil && entry.scope != nil {
		entry.scope.SetAttempt(attemptID)
	}
}

func (s *Service) report(c Child, p view.Progress) {
	if s.observe != nil {
		s.observe(c, p)
	}
}

func (s *Service) SetGate(g Gate)           { s.gate = g }
func (s *Service) SetEndpoints(e Endpoints) { s.endpoints = e }

func (s *Service) run(ctx context.Context, conversationID, delegatedBy string, parent, child task.Task,
	candidate roster.Candidate, req agentmcp.DelegateRequest, progress func(view.Progress)) (result agentmcp.DelegateResult, runErr error) {
	result = agentmcp.DelegateResult{TaskID: child.ID, Agent: candidate.Agent.ID, Node: candidate.Node}

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
				slog.Warn(fmt.Sprintf("delegate: node %q messaging endpoint: %v", candidate.Node, err), "task", child.ID, "parent", parent.ID, "conversation", conversationID, "agent", candidate.Agent.ID, "node", candidate.Node)
				endpoint = ""
			}
		}
		if candidate.Node == "" || endpoint != "" {
			extras = s.gate.Delegated(conversationID, candidate.Agent.ID, child.ID, delegatedBy, token, endpoint)
			defer func() {
				var detached *execution.RetainedObserverDetached
				if !errors.As(runErr, &detached) {
					s.gate.Revoke(token)
				}
			}()
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
		Deadline:  deadlineText(child, now),
	})
	if err != nil {
		return result, err
	}

	// The child's own budget is its hard limit, when it has one; without
	// one silence (MaxSilence), an explicit task stop, or service shutdown stops it.
	var cancel context.CancelFunc
	if child.Budget.MaxElapsed > 0 {
		ctx, cancel = context.WithTimeout(ctx, child.Budget.MaxElapsed)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	touch := func() {}
	if s.MaxSilence > 0 {
		var stop func()
		var clock idle.Context
		clock, stop, touch = idle.WithTimeout(ctx, s.MaxSilence)
		ctx = clock
		defer stop()
		if s.RegisterIdle != nil {
			defer s.RegisterIdle(candidate.Node, clock)()
		}
	}

	at := harness.Placement{Node: candidate.Node, Harness: candidate.Harness}
	accountingTask, err := s.tasks.Begin(child.ID, candidate.Agent.ID, orHub(candidate.Node, s.node), "")
	if err != nil {
		return result, err
	}
	workspace := project.Workspace{ID: s.worktreeID(child), Project: parent.ProjectID, Node: candidate.Node, Path: child.Workspace, Kind: project.KindWorktree}
	base := s.baseOf(workspace.ID)
	attemptID, turnID := ledgerAttemptID(workspace), "delegate/"+child.ID
	accountingToken := task.ExecutionToken{TaskID: accountingTask.ID, Epoch: accountingTask.ExecutionEpoch}
	if original := execution.Token(ctx); original != nil {
		accountingToken = *original
	}
	s.rememberAttempt(child.ID, attemptID)
	if err := s.tasks.BindAttempt(accountingToken, attemptID, turnID); err != nil {
		return result, fmt.Errorf("bind delegate accounting: %w", err)
	}
	prompt := payload.Render() + exec.ReportingContract + worktreeContract
	if caps.Instructions != "" {
		prompt = caps.Instructions + "\n\n" + prompt
	}
	d := &delegation{service: s, parent: parent, child: child, req: req, at: at}
	ask, askUser := d.handlers()
	run, err := lifecycle.Run(ctx, lifecycle.Options{
		Attempts: s.attempts, Roster: s.roster, Sessions: s.sessions, Workspaces: s.artifacts, Actor: "delegate",
		Spec: attempt.Spec{Execution: execution.Token(ctx),
			ID: attemptID, TaskID: child.ID, TurnID: turnID, Kind: attempt.KindDelegate,
			Project: parent.ProjectID, Node: candidate.Node, Harness: candidate.Harness, Agent: candidate.Agent.ID, Slots: candidate.Slots,
			Region: candidate.Region, CanonicalRegion: s.homeRegion(ctx, parent.ProjectID),
			Workspace: workspace, Scope: attempt.ScopePathSet, Base: base, By: delegatedBy, Requires: req.Requires,
		},
		Lost: func() {
			slog.Warn(fmt.Sprintf("delegate: attempt %s lost its lease; cancelling task #%s", attemptID, child.ID), "attempt", attemptID, "task", child.ID, "parent", parent.ID, "conversation", conversationID, "node", candidate.Node)
		},
		// The machine's final word on the requirement, taken now, before a
		// session is opened there.
		Candidate: candidate, Requires: req.Requires, Uses: candidate.Agent.MCPServers, AdmitUnsure: true,
		ArmActor: "delegate-opening",
		At:       at, Workdir: child.Workspace, Servers: caps.MCPServers,
		Model: candidate.Agent.Model, ModelOptions: candidate.Agent.Options,
		Prompt: prompt, Ask: ask, AskUser: askUser,
		Observe: func(p view.Progress) {
			touch()
			if progress != nil {
				progress(p)
			}
		},
		Arm: d.arm, Started: d.started, Finish: d.finish, Failed: d.failed, Wrap: d.wrap,
		// A hub session's close decides whether its stop is confirmed; a
		// node-owned session this process stops observing is the node's,
		// and stays recoverable unless the run itself was cancelled.
		Settlement: lifecycle.Settlement{Quarantine: lifecycle.QuarantineManaged, DetachManaged: true, Detachment: lifecycle.DetachQuarantinesUnlessCancelled, CancelDetaches: true},
	})
	return d.settle(ctx, run, err)
}

// delegation is one child's side of the lifecycle: who answers its
// questions, what its result publishes, and how the parent reads the end.
type delegation struct {
	service *Service
	parent  task.Task
	child   task.Task
	req     agentmcp.DelegateRequest
	at      harness.Placement

	mu      sync.Mutex
	binding QuestionBinding
	// published is the result on its way to the ledger, once Finish ran;
	// publishErr is why it did not get there.
	published  publication
	publishErr error
}

// arm remembers the session against the child for cancel and questions.
func (d *delegation) arm(_ context.Context, e *lifecycle.Execution) (func(*attempt.Record), error) {
	s := d.service
	s.mu.Lock()
	if entry := s.pending[d.child.ID]; entry != nil {
		entry.session = e.Session.ID()
	}
	s.mu.Unlock()
	record := e.Record
	record.Session = e.Session.ID()
	d.mu.Lock()
	d.binding = questionBinding(d.parent, d.child, record)
	d.mu.Unlock()
	return nil, nil
}

func (d *delegation) started(ctx context.Context, e *lifecycle.Execution) error {
	if !e.Managed {
		return nil
	}
	if err := d.service.bindDelegatedExecution(ctx, d.parent, d.child, e.Record); err != nil {
		return fmt.Errorf("bind delegated tools: %w", err)
	}
	return nil
}

// handlers are the child's question handlers, and nil where the service
// has none: a node-owned session reads a nil handler as no answer, not
// as a refusal.
func (d *delegation) handlers() (permission.AskFunc, func(context.Context, view.Question) (view.Answer, error)) {
	permissionHandler, questionHandler := d.service.nodeQuestionHandlers(QuestionBinding{})
	var ask permission.AskFunc
	var askUser func(context.Context, view.Question) (view.Answer, error)
	if permissionHandler != nil {
		ask = d.ask
	}
	if questionHandler != nil {
		askUser = d.askUser
	}
	return ask, askUser
}

func (d *delegation) ask(ctx context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
	d.mu.Lock()
	binding := d.binding
	d.mu.Unlock()
	ask, _ := d.service.nodeQuestionHandlers(binding)
	if ask == nil {
		return acp.RequestPermissionOutcome{}, errors.New("delegated permission handler is not configured")
	}
	return ask(ctx, q)
}

func (d *delegation) askUser(ctx context.Context, q view.Question) (view.Answer, error) {
	d.mu.Lock()
	binding := d.binding
	d.mu.Unlock()
	_, askUser := d.service.nodeQuestionHandlers(binding)
	if askUser == nil {
		return view.Answer{}, errors.New("delegated question handler is not configured")
	}
	return askUser(ctx, q)
}

func (d *delegation) finish(ctx context.Context, e *lifecycle.Execution) (attempt.Completion, error) {
	p, err := d.service.publish(ctx, d.child, e.Record, e.Outcome.Answer, e.Usage)
	d.published = p
	if err != nil {
		d.publishErr = err
		return attempt.Completion{}, err
	}
	return p.completion, nil
}

func (d *delegation) failed(e *lifecycle.Execution, cause error) (*attempt.Result, error) {
	if !e.Managed {
		return nil, nil
	}
	return retainedFailure(e.Record, e.Outcome.Answer, cause)
}

func (d *delegation) wrap(step lifecycle.Step, e *lifecycle.Execution, err error) error {
	switch step {
	case lifecycle.StepOpen:
		return fmt.Errorf("lease delegation: %w", err)
	case lifecycle.StepAdmit:
		var refused *lifecycle.Refused
		if errors.As(err, &refused) {
			return &Refusal{Code: "REFUSED_AT_ADMISSION", Retryable: true, Requires: d.req.Requires,
				Failures: []Failure{{Agent: d.child.Member, Node: d.at.Node, Reasons: refused.Admission.Atoms}}}
		}
		return fmt.Errorf("admission on %s: %w", d.at, err)
	case lifecycle.StepSession:
		if e.Session != nil || errors.Is(err, harness.ErrStopUnconfirmed) {
			return err
		}
		return fmt.Errorf("open session on %s: %w", d.at, err)
	case lifecycle.StepFinish:
		return fmt.Errorf("complete delegation result: %w", err)
	}
	return err
}

// settle reads how the run ended for the child's task and its parent.
func (d *delegation) settle(ctx context.Context, run lifecycle.Result, err error) (agentmcp.DelegateResult, error) {
	s, child, parent := d.service, d.child, d.parent
	record := run.Record
	result := d.published.result
	if result.TaskID == "" {
		result = agentmcp.DelegateResult{TaskID: child.ID, Agent: record.Agent, Node: record.Node}
	}
	if run.Driven {
		s.spent(child.ID, run.Last)
	}
	if run.CleanupErr != nil && run.Managed {
		slog.Error(fmt.Sprintf("delegate: settled session cleanup task=%s attempt=%s: %v", child.ID, record.ID, run.CleanupErr), "task", child.ID, "parent", parent.ID, "attempt", record.ID, "node", record.Node)
	}
	var step *lifecycle.StepError
	var detached *execution.RetainedObserverDetached
	switch {
	case errors.As(err, &detached):
		if run.Managed && run.Driven {
			result.Answer = run.Answer
		}
		return result, err
	case run.Unsettled && run.Session == nil:
		if opening := pendingDelegateOpen(record, child, err); opening != nil {
			opening.Cause = err
			s.reportDelegatePreparation(ctx, parent, child, record)
			return result, retainedDetached(record, opening)
		}
		s.finish(child.ID, task.OutcomeError)
		return result, err
	case errors.As(err, &step):
		outcome := task.OutcomeError
		if step.Step == lifecycle.StepSession && run.Session != nil {
			outcome = task.OutcomeCancelled
		}
		accountingErr := s.finish(child.ID, outcome)
		if step.Step == lifecycle.StepOpen || step.Step == lifecycle.StepArm {
			err = errors.Join(err, accountingErr)
		}
		return result, err
	case err != nil:
		if run.Managed {
			result.Answer = run.Answer
		}
		outcome := outcomeOf(err)
		if d.publishErr != nil {
			outcome = task.OutcomeError
		}
		s.finish(child.ID, outcome)
		return result, err
	}
	if run.Managed && ctx.Err() != nil {
		return result, retainedDetached(record, ctx.Err())
	}
	return s.land(ctx, parent, child, record, d.published)
}

// publication is a delegation's result on its way to the ledger: what the
// child said, the artifact its files became, and the completion binding
// that artifact to the child's name.
type publication struct {
	result     agentmcp.DelegateResult
	published  artifact.Manifest
	changed    bool
	completion attempt.Completion
}

// publish makes the child's files an artifact and walks the attempt to
// bind-ready with it. What the child said is the result from here on,
// whatever happens to its files: every return carries it.
func (s *Service) publish(ctx context.Context, child task.Task, record attempt.Record, answer string, observed *attempt.Usage) (publication, error) {
	p := publication{result: agentmcp.DelegateResult{TaskID: child.ID, Agent: record.Agent, Node: record.Node, Outcome: task.OutcomeOK, Answer: answer}}
	for _, ref := range exec.ParseRefs(answer) {
		p.result.Refs = append(p.result.Refs, ref.Kind+" "+ref.Value)
	}
	// The child's result becomes an artifact bound to its name and queued
	// to land once the parent's turn releases the canonical lock.
	published, changed, err := s.artifacts.Publish(ctx, record.Workspace, record.Base, record.ID, "delegation #"+child.ID)
	if err != nil {
		return p, fmt.Errorf("publish delegation result: %w", err)
	}
	for _, to := range []attempt.State{attempt.Snapshotted, attempt.Published, attempt.Durable, attempt.BindReady} {
		if _, err := s.attempts.Advance(ctx, record.ID, to, "delegate", nil); err != nil {
			return p, err
		}
	}
	name := "steve/" + child.ID + "/result"
	current, _, err := s.artifacts.Resolve(ctx, name)
	if err != nil {
		return p, err
	}
	if changed {
		p.result.Refs = append(p.result.Refs, "artifact "+published.ID)
	}
	output, err := json.Marshal(p.result)
	if err != nil {
		return p, err
	}
	p.published, p.changed = published, changed
	p.completion = attempt.Completion{Result: attempt.Result{Artifact: published.ID, Summary: text.Clip(p.result.Answer, 200), Refs: p.result.Refs, Output: output}, Usage: observed,
		Binding: &attempt.NameBinding{Name: name, ExpectedVersion: current.Version}}
	return p, nil
}

// land puts a committed result's change where the parent works, or
// queues it to land when the parent's turn is over.
func (s *Service) land(ctx context.Context, parent, child task.Task, record attempt.Record, p publication) (agentmcp.DelegateResult, error) {
	result := p.result
	if p.changed {
		// The parent asked for this and holds the canonical lock right now:
		// the result lands under its lease, into the directory it is
		// working in, so the parent sees the files this turn. A conflict
		// is queued for after the turn and reported as such.
		if lease, ok := s.parentLease(ctx, parent); ok {
			if pr, found, perr := s.artifacts.Project(ctx, parent.ProjectID); perr == nil && found {
				landed, lerr := s.artifacts.LandUnder(ctx, pr, p.published.ID, "task #"+child.ID, lease, artifact.SourceOf(ctx, record.ID)...)
				switch {
				case lerr == nil:
					result.Refs = append(result.Refs, fmt.Sprintf("landed into your working directory: %d path(s)", len(landed.Paths)))
					s.finish(child.ID, task.OutcomeOK)
					return result, nil
				default:
					slog.Warn(fmt.Sprintf("delegate: land %s under parent's lease: %v", p.published.ID, lerr), "artifact", p.published.ID, "task", child.ID, "parent", parent.ID, "attempt", record.ID, "project", parent.ProjectID)
					result.Refs = append(result.Refs, "not landed yet: "+lerr.Error())
				}
			}
		}
		if err := s.artifacts.Defer(ctx, parent.ProjectID, p.published.ID, "task #"+child.ID, artifact.SourceOf(ctx, record.ID)...); err != nil {
			if lifecycle.IsManaged(record.Session) {
				return result, retainedDetached(record, err)
			}
			s.finish(child.ID, task.OutcomeError)
			return result, fmt.Errorf("queue landing: %w", err)
		}
		result.Refs = append(result.Refs, "queued to land; Steve lands it and tells you when it has")
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

func (s *Service) finish(id string, outcome task.Outcome) error {
	tracked, ok := s.tasks.Get(id)
	if !ok || len(tracked.Attempts) == 0 {
		return fmt.Errorf("task %s has no accepted attempt", id)
	}
	s.mu.Lock()
	spent := s.spends[id]
	s.mu.Unlock()
	tokens := task.FromUsage(spent.Usage.InputTokens, spent.Usage.OutputTokens, spent.Usage.CacheReadTokens, spent.Usage.CacheWriteTokens)
	if attemptID := s.attemptOf(id); attemptID != "" {
		for _, row := range tracked.Attempts {
			if row.ExecutionID == attemptID {
				if err := s.tasks.SettleAttempt(id, attemptID, row.TurnID, time.Time{}, outcome, task.RecoveryUsage{Tokens: tokens, Model: spent.Settings.Model, Reported: spent.Usage.TokensReported()}); err != nil {
					return fmt.Errorf("settle delegate task #%s: %w", id, err)
				}
				s.mu.Lock()
				delete(s.spends, id)
				s.mu.Unlock()
				return nil
			}
		}
	}
	return errors.New("delegate execution has no exact task accounting row")
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
	case errors.Is(err, harness.ErrTurnCanceled) || ctxErr(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

func ctxErr(err, target error) bool {
	return err == target || strings.Contains(err.Error(), target.Error())
}

const goalLimit = 120

// goal is the first line of the request, cut to fit a task listing.
func goal(request string) string {
	line := strings.TrimSpace(text.FirstLine(strings.TrimSpace(request)))
	return text.Clip(line, goalLimit)
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

// discardUnused removes the worktree a delegation was given before it
// was refused. The caller is already returning the refusal, which is the
// error the agent needs; a worktree that could not be removed is disk
// left on the node, reported here for the operator.
func (s *Service) discardUnused(ctx context.Context, workspace project.Workspace, parent task.Task) {
	if err := s.artifacts.Discard(context.WithoutCancel(ctx), workspace); err != nil {
		// No attempt was ever opened on this worktree, so the workspace
		// id is the only identifier that leads anywhere.
		slog.Warn(fmt.Sprintf("delegate: discard unused workspace %s: %v", workspace.Path, err), "workspace", workspace.ID, "parent", parent.ID, "node", workspace.Node, "project", workspace.Project)
	}
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

// ledgerAttemptID is the attempt a child's run is recorded under. The
// materializer names the worktree after the attempt it was cut for, so
// the id the ledger sees is the workspace id without its "wt-" prefix --
// not the minted att- id, which only names the workspace request.
func ledgerAttemptID(workspace project.Workspace) string {
	return strings.TrimPrefix(workspace.ID, "wt-")
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

func attemptUsage(p view.Progress) *attempt.Usage { return lifecycle.Usage(p) }

func (s *Service) advanceExecution(ctx context.Context, id string, to task.State) (task.Task, error) {
	if tracked, ok := s.tasks.Get(id); ok && tracked.State == to {
		if token := execution.Token(ctx); token != nil {
			return tracked, s.tasks.CheckExecution(*token)
		}
		return tracked, nil
	}
	if token := execution.Token(ctx); token != nil {
		return s.tasks.AdvanceExecution(*token, to)
	}
	return s.tasks.Advance(id, to)
}

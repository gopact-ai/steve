// Package agentexec runs one bounded planning or verification prompt with
// the same admission, accounting and cancellation boundaries as other work.
package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/view"
)

type Sessions interface {
	OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error)
	CloseSession(context.Context, harness.Placement, string) error
}
type Workspaces interface {
	Materialize(context.Context, project.Request) (project.Workspace, error)
	Discard(context.Context, project.Workspace) error
}
type Budget interface {
	Reserve(string) (int, time.Time, error)
}

type Capabilities interface {
	Assemble(roster.Candidate) (string, []acp.MCPServer, error)
}

type Spec struct {
	TaskID, TurnID, Agent, Project, Base string
	Kind                                 attempt.Kind
	Timeout                              time.Duration
	Source                               json.RawMessage
}
type Result struct {
	Answer  string
	Attempt attempt.Record
	Usage   *attempt.Usage
}

// ValidationError is returned only after invalid model output has been
// durably settled. Its caller may request a correction without retrying an
// execution whose cleanup or completion failed.
type ValidationError struct{ Cause error }

func (e *ValidationError) Error() string { return e.Cause.Error() }
func (e *ValidationError) Unwrap() error { return e.Cause }

// UnsettledError identifies the actual auxiliary writer. A parent executor
// must not quarantine its own workspace for a verifier's unconfirmed stop.
type UnsettledError struct {
	AttemptID string
	Cause     error
}

func (e *UnsettledError) Error() string            { return fmt.Sprintf("attempt %s: %v", e.AttemptID, e.Cause) }
func (e *UnsettledError) Unwrap() error            { return e.Cause }
func (e *UnsettledError) UnsettledAttempt() string { return e.AttemptID }

type Runner struct {
	sessions     Sessions
	roster       *roster.Roster
	workspaces   Workspaces
	attempts     *attempt.Service
	executions   *execution.Registry
	budget       Budget
	capabilities Capabilities
	slotPoll     time.Duration
	mu           sync.Mutex
	active       map[string]bool
	ask          permission.AskFunc
	askUser      acphost.AskUserFunc
	observe      func(attempt.Record, view.Progress)
}

func New(sessions Sessions, fleet *roster.Roster, workspaces Workspaces, attempts *attempt.Service, executions *execution.Registry, budget Budget) *Runner {
	return &Runner{sessions: sessions, roster: fleet, workspaces: workspaces, attempts: attempts, executions: executions, budget: budget, slotPoll: 3 * time.Second}
}

// SetCapabilities configures the ordinary guest instructions and MCP servers
// before the runner is used. Admission still binds node-owned servers itself.
func (r *Runner) SetCapabilities(c Capabilities) { r.capabilities = c }

func (r *Runner) Prompt(parent context.Context, spec Spec, prompt string, validate func(string) error) (out Result, runErr error) {
	if spec.Kind != attempt.KindPlan && spec.Kind != attempt.KindVerify {
		return out, errors.New("agentexec supports only planning and verification")
	}
	if spec.TaskID == "" || spec.Agent == "" || spec.Project == "" {
		return out, errors.New("agentexec requires task, agent and project")
	}
	if r == nil || r.sessions == nil || r.roster == nil || r.workspaces == nil || r.attempts == nil || r.executions == nil || r.budget == nil {
		return out, errors.New("agentexec execution dependencies are not configured")
	}
	original := auxiliaryInput{Spec: spec, Prompt: prompt}
	identity, err := workID(spec, prompt)
	if err != nil {
		return out, err
	}
	key := auxiliaryKey(spec)
	if !r.claim(key) {
		return out, Blocked(attempt.Record{Spec: attempt.Spec{TaskID: spec.TaskID}}, "busy", "检查原执行占用", "相同规划或验证请求已有观察者。", "建议等待这次执行完成后核对。", nil)
	}
	defer r.release(key)
	if spec.TurnID != "" {
		prior, found, err := r.attempts.LatestForTurn(parent, spec.TurnID)
		if err != nil {
			return out, errors.Join(err, parent.Err())
		}
		if found && prior.Kind == spec.Kind && (strings.HasPrefix(prior.Session, "ns_") || PendingOpen(prior)) {
			if prior.TaskID != spec.TaskID || prior.WorkID != identity {
				return out, Blocked(prior, "work", "核对原规划或验证请求", "同一请求标识对应的输入条件已经变化。", "建议核对原任务与已有执行，不重发原命令。", nil)
			}
			return r.resumeAttempt(parent, prior, validate)
		}
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// Dependencies such as SQLite can return their own interruption error
	// when a prompt expires. Keep both errors so callers can recognize the
	// cancellation without losing evidence of a failed or uncertain operation.
	// Capture this context: child contexts are also canceled during cleanup.
	defer func(promptCtx context.Context) {
		if runErr != nil && promptCtx.Err() != nil && !errors.Is(runErr, promptCtx.Err()) {
			runErr = errors.Join(runErr, promptCtx.Err())
		}
	}(ctx)
	id := attempt.NewID()
	scope, err := r.executions.Begin(ctx, execution.Key{TaskID: spec.TaskID, InstanceID: spec.TurnID, AttemptID: id})
	if err != nil {
		return out, err
	}
	var unresolved error
	var invalid error
	validationReady := false
	defer func() {
		if validationReady {
			runErr = &ValidationError{Cause: invalid}
		}
		scope.Finish(unresolved)
	}()
	ctx = scope.Context()
	var selected roster.Candidate
	found := false
	for _, c := range r.roster.All(ctx) {
		if c.Agent.ID == spec.Agent {
			selected, found = c, true
			break
		}
	}
	if !found {
		return out, fmt.Errorf("agent %s is not in the roster", spec.Agent)
	}
	if !selected.Eligible {
		return out, fmt.Errorf("agent %s cannot run: %s", spec.Agent, selected.Why)
	}
	workspace, err := r.workspaces.Materialize(ctx, project.Request{Project: spec.Project, Node: selected.Node, Isolated: true, Base: spec.Base, Owner: id})
	if err != nil {
		return out, fmt.Errorf("materialize %s: %w", spec.Kind, err)
	}
	keepWorkspace := false
	defer func() {
		if keepWorkspace {
			return
		}
		cleanup, stop := lifecycle.Cleanup(ctx)
		defer stop()
		if err := r.workspaces.Discard(cleanup, workspace); err != nil {
			validationReady = false
			runErr = errors.Join(runErr, fmt.Errorf("discard %s: %w", id, err))
		}
	}()
	if workspace.Kind != project.KindWorktree {
		keepWorkspace = true
		return out, errors.New("agentexec requires an isolated worktree")
	}
	request := attempt.Spec{WorkID: identity, ID: id, TaskID: spec.TaskID, TurnID: spec.TurnID, Kind: spec.Kind, Execution: execution.Token(ctx), Project: spec.Project,
		Node: selected.Node, Harness: selected.Harness, Agent: selected.Agent.ID, Slots: selected.Slots, Region: selected.Region,
		Workspace: workspace, Scope: attempt.ScopeNone, Base: workspace.Base, By: "agentexec", Requires: selected.Agent.Requires}
	for {
		out.Attempt, err = r.attempts.Open(ctx, request)
		var full attempt.NoSlot
		if !errors.As(err, &full) {
			break
		}
		timer := time.NewTimer(r.slotPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		return out, err
	}
	ctx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	defer lifecycle.Keep(ctx, r.attempts, id, stopRun)()
	_, deadline, reserveErr := ReserveBudget(r.budget, out.Attempt)
	if reserveErr != nil {
		cleanup, stop := lifecycle.Cleanup(ctx)
		defer stop()
		failed, settleErr := r.attempts.FailWith(cleanup, id, "agentexec", reserveErr.Error(), nil)
		if settleErr != nil {
			keepWorkspace = true
		} else {
			out.Attempt = failed
		}
		return out, errors.Join(reserveErr, settleErr)
	}
	if !deadline.IsZero() {
		var cancelBudget context.CancelFunc
		ctx, cancelBudget = context.WithDeadline(ctx, deadline)
		defer cancelBudget()
	}
	at := harness.Placement{Node: selected.Node, Harness: selected.Harness}
	var promptSettled bool
	var session harness.Runner
	var releaseBindings bool
	defer func() {
		if session != nil && lifecycle.Managed(session) || !out.Attempt.State.Terminal() || out.Attempt.Unsettled {
			return
		}
		if err := SettleBudget(r.budget, out.Attempt, runErr); err != nil {
			validationReady = false
			runErr = Blocked(out.Attempt, "accounting", "保存原执行的用量与预算", "执行结果已保存，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", err)
		}
	}()
	defer func() {
		if session != nil && lifecycle.Managed(session) {
			keepWorkspace = true
			if out.Attempt.Session == "" {
				// The node returned a real session, but publishing that identity
				// failed before any prompt. Preserve the admitted preparation;
				// this observer may leave without claiming native process exit.
				cleanup, cancel := lifecycle.Cleanup(ctx)
				defer cancel()
				markerErr := r.attempts.MarkUnsettled(cleanup, out.Attempt.ID, "agentexec-open", errors.Join(harness.ErrStopUnconfirmed, runErr), out.Usage)
				unresolved = &execution.NodePreparationObserverDetached{AttemptID: out.Attempt.ID, NodeID: out.Attempt.Node, OpenCommandID: attempt.InputCommandID(out.Attempt) + "/open", Cause: errors.Join(harness.ErrStopUnconfirmed, runErr, markerErr)}
				runErr = Blocked(out.Attempt, "session-record", "保存原节点已返回的会话标识", "节点已经打开会话，但会话标识尚未写入执行记录；原始任务输入还未发送。", "建议恢复存储后核对原节点的打开回执，不重新打开会话。", unresolved)
				return
			}
			out, runErr, unresolved = r.finishManaged(ctx, out, runErr, invalid, promptSettled)
			return
		}
		cleanup, stop := lifecycle.Cleanup(ctx)
		defer stop()
		if !errors.Is(runErr, harness.ErrStopUnconfirmed) && session != nil {
			if err := lifecycle.Close(cleanup, r.sessions, at, session); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		if errors.Is(runErr, harness.ErrStopUnconfirmed) {
			keepWorkspace = true
			quarantine := r.attempts.MarkUnsettled(cleanup, id, "agentexec", runErr, out.Usage)
			unresolved = &UnsettledError{AttemptID: id, Cause: errors.Join(runErr, quarantine)}
			if pending := PendingNodeOpen(out.Attempt, runErr); pending != nil {
				unresolved = pending
			}
			runErr = unresolved
			if record, err := r.attempts.Get(cleanup, id); err == nil {
				out.Attempt = record
			}
			if PendingOpen(out.Attempt) {
				runErr = Blocked(out.Attempt, "open", "按原执行标识请求打开节点会话", "原节点未返回完整的打开回执，会话可能已经创建。", "建议恢复原节点连接后核对打开记录，保留原任务等待处理。", unresolved)
			}
			return
		}
		if session != nil {
			if promptSettled || lifecycle.Stopped(session) {
				if err := r.attempts.MarkSessionSettled(cleanup, id, "agentexec"); err != nil {
					keepWorkspace = true
					runErr = errors.Join(runErr, err)
					return
				}
			}
		}
		if releaseBindings {
			r.roster.Release(cleanup, selected.Node, id)
		}
		if runErr == nil && ctx.Err() != nil {
			runErr = ctx.Err()
		}
		if runErr != nil {
			failed, err := r.attempts.FailWith(cleanup, id, "agentexec", runErr.Error(), out.Usage)
			if err != nil {
				keepWorkspace = true
				runErr = errors.Join(runErr, fmt.Errorf("settle %s: %w", id, err))
				return
			}
			out.Attempt = failed
			validationReady = invalid != nil
			return
		}
		summary := []rune(out.Answer)
		if len(summary) > 200 {
			summary = summary[:200]
		}
		completion := attempt.Completion{Result: attempt.Result{Summary: string(summary)}, Usage: out.Usage}
		completed, err := r.attempts.FinishCompletion(ctx, id, "agentexec", completion)
		if err != nil {
			keepWorkspace = true
			runErr = r.attempts.RejectCompletion(cleanup, id, "agentexec", completion, err)
			if record, err := r.attempts.Get(cleanup, id); err == nil {
				out.Attempt = record
			}
			return
		}
		out.Attempt = completed
	}()
	admission, bindings, err := r.roster.Admit(ctx, selected, selected.Agent.Requires, selected.Agent.MCPServers, id)
	if err != nil {
		return out, fmt.Errorf("admit %s: %w", spec.Agent, err)
	}
	if !admission.OK() {
		return out, fmt.Errorf("admit %s: %s", spec.Agent, admission.Unmet())
	}
	releaseBindings = len(bindings) > 0
	var servers []acp.MCPServer
	if r.capabilities != nil {
		instructions, configured, err := r.capabilities.Assemble(selected)
		if err != nil {
			return out, fmt.Errorf("assemble %s: %w", spec.Agent, err)
		}
		servers = configured
		if instructions != "" {
			prompt = instructions + "\n\n" + prompt
		}
	} else if selected.Node == "" && len(selected.Agent.MCPServers) > 0 {
		return out, errors.New("hub MCP capabilities are not configured for agentexec")
	}
	input, encodeErr := json.Marshal(auxiliaryOutput{WorkID: identity, Input: &original})
	if encodeErr != nil {
		return out, encodeErr
	}
	out.Attempt, err = r.attempts.Advance(ctx, id, attempt.Prepared, "agentexec", func(record *attempt.Record) {
		record.Admission = &admission
		record.Result = &attempt.Result{Output: input}
	})
	if err != nil {
		return out, err
	}
	if err := r.attempts.ArmSession(ctx, id, "agentexec-open"); err != nil {
		return out, err
	}
	session, err = r.sessions.OpenSession(ctx, at, "", workspace.Path, append(servers, roster.ToMCP(bindings)...))
	if err != nil {
		if !errors.Is(err, harness.ErrStopUnconfirmed) {
			if markerErr := r.attempts.MarkSessionSettled(ctx, id, "agentexec-open-rejected"); markerErr != nil {
				return out, errors.Join(err, markerErr)
			}
		}
		return out, fmt.Errorf("open %s: %w", spec.Agent, err)
	}
	harness.ApplyPreferences(ctx, session, selected.Agent.ID, selected.Agent.Model, selected.Agent.Options)
	running, err := r.attempts.Advance(ctx, id, attempt.Running, "agentexec", func(record *attempt.Record) { record.Session = session.ID() })
	if err != nil {
		return out, err
	}
	out.Attempt = running
	ask, askUser, observe := r.callbacks()
	driven := lifecycle.Drive{Session: session, Prompt: prompt, Turn: lifecycle.Managed(session), Ask: ask, AskUser: askUser, Observe: func(p view.Progress) {
		EmitProgress(ctx, p)
		if observe != nil {
			observe(out.Attempt, p)
		}
	}}.Run(ctx)
	out.Answer, runErr = driven.Answer, driven.Err
	promptSettled = driven.Settled()
	if !promptSettled {
		runErr = errors.Join(runErr, harness.ErrStopUnconfirmed)
	}
	out.Usage = lifecycle.Usage(driven.Last)
	if !lifecycle.Managed(session) {
		out.Answer = strings.TrimSpace(out.Answer)
	}
	if runErr == nil && ctx.Err() != nil {
		runErr = ctx.Err()
	}
	if runErr == nil && validate != nil {
		invalid = validate(out.Answer)
		runErr = invalid
	}
	return out, runErr
}

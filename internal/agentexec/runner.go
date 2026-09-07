// Package agentexec runs one bounded planning or verification prompt with
// the same admission, accounting and cancellation boundaries as other work.
package agentexec

import (
	"context"
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
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
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
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
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
	request := attempt.Spec{ID: id, TaskID: spec.TaskID, TurnID: spec.TurnID, Kind: spec.Kind, Execution: execution.Token(ctx), Project: spec.Project,
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
	heartbeatCtx := ctx
	lost := r.attempts.Heartbeat(heartbeatCtx, id)
	go func() {
		select {
		case <-lost:
			stopRun()
		case <-heartbeatCtx.Done():
		}
	}()
	_, deadline, reserveErr := r.budget.Reserve(spec.TaskID)
	if reserveErr != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
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
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		if !errors.Is(runErr, harness.ErrStopUnconfirmed) && session != nil {
			if err := r.sessions.CloseSession(cleanup, at, session.ID()); err != nil {
				stopped, ok := session.(interface{ Stopped() bool })
				if !ok || !stopped.Stopped() {
					runErr = errors.Join(runErr, harness.ErrStopUnconfirmed, err)
				}
			}
		}
		if errors.Is(runErr, harness.ErrStopUnconfirmed) {
			keepWorkspace = true
			quarantine := r.attempts.MarkUnsettled(cleanup, id, "agentexec", runErr, out.Usage)
			unresolved = &UnsettledError{AttemptID: id, Cause: errors.Join(runErr, quarantine)}
			runErr = unresolved
			if record, err := r.attempts.Get(cleanup, id); err == nil {
				out.Attempt = record
			}
			return
		}
		if session != nil {
			stopped, ok := session.(interface{ Stopped() bool })
			if promptSettled || (ok && stopped.Stopped()) {
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
	out.Attempt, err = r.attempts.Advance(ctx, id, attempt.Prepared, "agentexec", func(record *attempt.Record) { record.Admission = &admission })
	if err != nil {
		return out, err
	}
	session, err = r.sessions.OpenSession(ctx, at, "", workspace.Path, append(servers, roster.ToMCP(bindings)...))
	if err != nil {
		return out, fmt.Errorf("open %s: %w", spec.Agent, err)
	}
	harness.ApplyPreferences(ctx, session, selected.Agent.ID, selected.Agent.Model, selected.Agent.Options)
	out.Attempt, err = r.attempts.Advance(ctx, id, attempt.Running, "agentexec", nil)
	if err != nil {
		return out, err
	}
	var mu sync.Mutex
	var last view.Progress
	out.Answer, _, runErr = session.Prompt(ctx, prompt, func(p view.Progress) { mu.Lock(); last = p; mu.Unlock() })
	promptSettled = acphost.PromptSettled(runErr)
	if stopped, ok := session.(interface{ Stopped() bool }); ok && stopped.Stopped() {
		promptSettled = true
	}
	if !promptSettled {
		runErr = errors.Join(runErr, harness.ErrStopUnconfirmed)
	}
	mu.Lock()
	u := last.Usage
	out.Usage = &attempt.Usage{Model: last.Settings.Model, Input: int64(u.InputTokens), Output: int64(u.OutputTokens), CachedRead: int64(u.CacheReadTokens), CachedWrite: int64(u.CacheWriteTokens), Context: int64(u.ContextTokens), Reported: u.TokensReported()}
	mu.Unlock()
	out.Answer = strings.TrimSpace(out.Answer)
	if runErr == nil && ctx.Err() != nil {
		runErr = ctx.Err()
	}
	if runErr == nil && validate != nil {
		invalid = validate(out.Answer)
		runErr = invalid
	}
	return out, runErr
}

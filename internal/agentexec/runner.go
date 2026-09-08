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
	defer func() { scope.Finish(unresolved) }()
	ctx = scope.Context()
	selected, err := r.candidate(ctx, spec.Agent)
	if err != nil {
		return out, err
	}
	workspace, err := r.workspaces.Materialize(ctx, project.Request{Project: spec.Project, Node: selected.Node, Isolated: true, Base: spec.Base, Owner: id})
	if err != nil {
		return out, fmt.Errorf("materialize %s: %w", spec.Kind, err)
	}
	if workspace.Kind != project.KindWorktree {
		return out, errors.New("agentexec requires an isolated worktree")
	}
	work := &auxiliary{runner: r, spec: spec, selected: selected, identity: identity, input: original, validate: validate}
	defer work.stop()
	run, err := lifecycle.Run(ctx, work.options(ctx, id, workspace))
	out.Attempt, out.Usage, out.Answer = run.Record, run.Usage, run.Answer
	if !run.Managed {
		out.Answer = strings.TrimSpace(out.Answer)
	}
	runErr, unresolved = work.settle(run, err)
	return out, runErr
}

// candidate is the roster's word on the agent the caller named.
func (r *Runner) candidate(ctx context.Context, agentID string) (roster.Candidate, error) {
	for _, c := range r.roster.All(ctx) {
		if c.Agent.ID == agentID {
			if !c.Eligible {
				return c, fmt.Errorf("agent %s cannot run: %s", agentID, c.Why)
			}
			return c, nil
		}
	}
	return roster.Candidate{}, fmt.Errorf("agent %s is not in the roster", agentID)
}

// auxiliary is one planning or verification prompt's side of the
// lifecycle: what it prepares, how it checks the answer, what it records,
// and how it reads the end.
type auxiliary struct {
	runner   *Runner
	spec     Spec
	selected roster.Candidate
	identity string
	input    auxiliaryInput
	validate func(string) error

	mu       sync.Mutex
	running  attempt.Record
	reserved bool
	invalid  error
	deadline context.CancelFunc
}

func (a *auxiliary) options(ctx context.Context, id string, workspace project.Workspace) lifecycle.Options {
	r, selected, spec := a.runner, a.selected, a.spec
	ask, askUser, observe := r.callbacks()
	return lifecycle.Options{
		Attempts: r.attempts, Roster: r.roster, Sessions: r.sessions, Workspaces: r.workspaces,
		Actor: "agentexec", SlotPoll: r.slotPoll,
		Spec: attempt.Spec{WorkID: a.identity, ID: id, TaskID: spec.TaskID, TurnID: spec.TurnID, Kind: spec.Kind, Execution: execution.Token(ctx), Project: spec.Project,
			Node: selected.Node, Harness: selected.Harness, Agent: selected.Agent.ID, Slots: selected.Slots, Region: selected.Region,
			Workspace: workspace, Scope: attempt.ScopeNone, Base: workspace.Base, By: "agentexec", Requires: selected.Agent.Requires},
		Candidate: selected, Requires: selected.Agent.Requires, Uses: selected.Agent.MCPServers,
		ArmActor: "agentexec-open",
		At:       harness.Placement{Node: selected.Node, Harness: selected.Harness}, Workdir: workspace.Path,
		Model: selected.Agent.Model, ModelOptions: selected.Agent.Options,
		Prompt: a.input.Prompt, Ask: ask, AskUser: askUser,
		Observe: func(p view.Progress) {
			EmitProgress(ctx, p)
			if observe != nil {
				a.mu.Lock()
				record := a.running
				a.mu.Unlock()
				observe(record, p)
			}
		},
		Leased: a.reserve, Prepare: a.prepare, Started: a.started, Validate: a.check, Finish: a.finish, Failed: a.failed, Wrap: a.wrap,
		// A planning or verification prompt is its own writer: an end it
		// cannot prove is an unconfirmed stop, and a node-owned session it
		// can no longer observe is the node's to finish.
		Settlement: auxiliarySettlement,
	}
}

// auxiliarySettlement: a planning or verification prompt is its own
// writer. An end it cannot prove is an unconfirmed stop; a node-owned
// session it can no longer observe, or was cancelled away from, is the
// node's to finish, and the record is left as it is for the observer
// that comes back.
var auxiliarySettlement = lifecycle.Settlement{StoppedSettles: true, Quarantine: lifecycle.QuarantineAlways, DetachManaged: true, Detachment: lifecycle.DetachSilently, CancelDetaches: true, QuarantineUnpublished: true}

// reserve charges the task's budget once the attempt is leased, and bounds
// the run by the budget's deadline when it has one.
func (a *auxiliary) reserve(ctx context.Context, e *lifecycle.Execution) (context.Context, error) {
	_, deadline, err := ReserveBudget(a.runner.budget, e.Record)
	if err != nil {
		return ctx, err
	}
	a.reserved = true
	if !deadline.IsZero() {
		ctx, a.deadline = context.WithDeadline(ctx, deadline)
	}
	return ctx, nil
}

func (a *auxiliary) stop() {
	if a.deadline != nil {
		a.deadline()
	}
}

// prepare assembles the guest's instructions and servers and keeps the
// original request on the record, so a retained execution can prove
// later what it was asked.
func (a *auxiliary) prepare(_ context.Context, e *lifecycle.Execution) (func(*attempt.Record), error) {
	r := a.runner
	if r.capabilities != nil {
		instructions, configured, err := r.capabilities.Assemble(a.selected)
		if err != nil {
			return nil, fmt.Errorf("assemble %s: %w", a.spec.Agent, err)
		}
		e.Servers = configured
		if instructions != "" {
			e.Prompt = instructions + "\n\n" + e.Prompt
		}
	} else if a.selected.Node == "" && len(a.selected.Agent.MCPServers) > 0 {
		return nil, errors.New("hub MCP capabilities are not configured for agentexec")
	}
	input, err := json.Marshal(auxiliaryOutput{WorkID: a.identity, Input: &a.input})
	if err != nil {
		return nil, err
	}
	return func(record *attempt.Record) { record.Result = &attempt.Result{Output: input} }, nil
}

func (a *auxiliary) started(_ context.Context, e *lifecycle.Execution) error {
	a.mu.Lock()
	a.running = e.Record
	a.mu.Unlock()
	return nil
}

// answer is the harness's reply as the caller sees it: a hub session's is
// trimmed, a node-owned session's kept verbatim for its record.
func (a *auxiliary) answer(e *lifecycle.Execution) string {
	if e.Managed {
		return e.Outcome.Answer
	}
	return strings.TrimSpace(e.Outcome.Answer)
}

func (a *auxiliary) check(e *lifecycle.Execution) error {
	if a.validate == nil {
		return nil
	}
	a.invalid = a.validate(a.answer(e))
	return a.invalid
}

func (a *auxiliary) finish(_ context.Context, e *lifecycle.Execution) (attempt.Completion, error) {
	answer := a.answer(e)
	result := attempt.Result{Summary: clipAnswer(answer)}
	if e.Managed {
		output, err := a.output(e.Record, answer, nil)
		if err != nil {
			return attempt.Completion{}, err
		}
		result.Output = output
	}
	return attempt.Completion{Result: result, Usage: e.Usage}, nil
}

// failed keeps a node-owned session's answer and error with the record: a
// retained execution rebuilds its result from there.
func (a *auxiliary) failed(e *lifecycle.Execution, cause error) (*attempt.Result, error) {
	if !e.Managed {
		return nil, nil
	}
	answer := a.answer(e)
	output, err := a.output(e.Record, answer, cause)
	if err != nil {
		return nil, err
	}
	return &attempt.Result{Summary: clipAnswer(answer), Output: output}, nil
}

var (
	errOriginalInput = errors.New("original auxiliary input")
	errOutput        = errors.New("auxiliary output")
)

func (a *auxiliary) output(record attempt.Record, answer string, cause error) ([]byte, error) {
	input, err := originalInput(record)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errOriginalInput, err)
	}
	saved := auxiliaryOutput{Input: &input, WorkID: record.WorkID, Answer: answer, Validation: a.invalid != nil}
	if cause != nil {
		saved.Error = cause.Error()
	}
	output, err := json.Marshal(saved)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errOutput, err)
	}
	return output, nil
}

// wrap names the agent in a failed admission or open, as the record and
// the caller both see it.
func (a *auxiliary) wrap(step lifecycle.Step, _ *lifecycle.Execution, err error) error {
	switch step {
	case lifecycle.StepAdmit:
		var refused *lifecycle.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("admit %s: %s", a.spec.Agent, refused.Admission.Unmet())
		}
		return fmt.Errorf("admit %s: %w", a.spec.Agent, err)
	case lifecycle.StepSession:
		return fmt.Errorf("open %s: %w", a.spec.Agent, err)
	}
	return err
}

// settle reads how the run ended into this caller's words: what it
// returns, and what stays unresolved for the execution scope.
func (a *auxiliary) settle(run lifecycle.Result, err error) (runErr, unresolved error) {
	record := run.Record
	var step *lifecycle.StepError
	errors.As(err, &step)
	var detached *execution.RetainedObserverDetached
	switch {
	case run.Unsettled && run.Managed && !run.Driven:
		// The node returned a real session, but publishing that identity
		// failed before any prompt. Preserve the admitted preparation;
		// this observer may leave without claiming native process exit.
		unresolved = &execution.NodePreparationObserverDetached{AttemptID: record.ID, NodeID: record.Node, OpenCommandID: attempt.InputCommandID(record) + "/open", Cause: err}
		return Blocked(record, "session-record", "保存原节点已返回的会话标识", "节点已经打开会话，但会话标识尚未写入执行记录；原始任务输入还未发送。", "建议恢复存储后核对原节点的打开回执，不重新打开会话。", unresolved), unresolved
	case run.Unsettled && record.State.Terminal():
		// The result is committed, but the close did not confirm the
		// process exited: the worktree, the slot and the budget wait for
		// someone who can.
		unresolved = &UnsettledError{AttemptID: record.ID, Cause: errors.Join(err, run.CleanupErr)}
		return Blocked(record, "cleanup", "释放已结束执行的工作区", "执行结果已保存，但原进程未确认退出，工作区尚未释放。", "建议核对原节点与进程，确认停止后重新检查。", unresolved), unresolved
	case run.Unsettled:
		unresolved = &UnsettledError{AttemptID: record.ID, Cause: err}
		if pending := PendingNodeOpen(record, err); pending != nil {
			unresolved = pending
		}
		if PendingOpen(record) {
			return Blocked(record, "open", "按原执行标识请求打开节点会话", "原节点未返回完整的打开回执，会话可能已经创建。", "建议恢复原节点连接后核对打开记录，保留原任务等待处理。", unresolved), unresolved
		}
		return unresolved, unresolved
	case errors.As(err, &detached):
		code := "observer"
		switch {
		case step != nil && step.Step == lifecycle.StepSettle:
			code = "marker"
		case step != nil && step.Step == lifecycle.StepFinish:
			code = "completion"
			if errors.Is(err, errOriginalInput) {
				code = "input"
			} else if errors.Is(err, errOutput) {
				code = "output"
			}
		}
		return Blocked(record, code, "保存原规划或验证执行的结果", "原执行或其结果尚未完整确认。", "建议恢复节点与存储后检查同一次执行。", detached), detached
	}
	if !record.State.Terminal() {
		return err, nil
	}
	if a.reserved {
		if budgetErr := SettleBudget(a.runner.budget, record, err); budgetErr != nil {
			return Blocked(record, "accounting", "保存原执行的用量与预算", "执行结果已保存，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", budgetErr), nil
		}
	}
	if run.CleanupErr != nil {
		if run.Managed {
			return Blocked(record, "cleanup", "释放已结束执行的工作区", "执行结果已保存，但原工作区尚未释放。", "建议恢复节点连接后重新检查。", run.CleanupErr), nil
		}
		return errors.Join(err, run.CleanupErr), nil
	}
	if a.invalid != nil && errors.Is(err, a.invalid) && run.Durable {
		// Invalid model output is durably settled: the caller may ask for
		// a correction without retrying an execution whose cleanup or
		// completion failed.
		return &ValidationError{Cause: a.invalid}, nil
	}
	return err, nil
}

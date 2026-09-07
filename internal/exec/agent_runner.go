package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Sessions opens an agent session wherever a step was placed. The harness
// manager satisfies it; the executor only needs this much of it.
type Sessions interface {
	OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error)
	CloseSession(ctx context.Context, at harness.Placement, upstreamID string) error
}

// Capabilities assembles the instructions and MCP servers a session starts
// with. The capability assembler satisfies it.
type Capabilities interface {
	Assemble(selected roster.Candidate) (instructions string, servers []acp.MCPServer, err error)
}

// AgentRunner runs a plan step by opening a session on the placed agent and
// prompting it with the assembled context.
//
// Each step gets a fresh session on purpose. A step is a unit of work with a
// stated goal and a bounded payload; carrying a previous step's conversation
// into it would smuggle in exactly the unbounded history the context object
// exists to prevent, and would tie the step to one machine.
type AgentRunner struct {
	sessions Sessions
	caps     Capabilities
	roster   *roster.Roster
	// Timeout bounds one step. Zero takes the default.
	Timeout time.Duration
	// observe sees each step's progress as it streams: what the agent is
	// thinking and calling, for whoever is watching the plan run.
	observe func(StepRequest, view.Progress)
	mu      sync.Mutex
	ask     permission.AskFunc
	askUser acphost.AskUserFunc
}

func NewAgentRunner(sessions Sessions, caps Capabilities, r *roster.Roster) *AgentRunner {
	return &AgentRunner{sessions: sessions, caps: caps, roster: r}
}

const DefaultStepTimeout = 15 * time.Minute

func (a *AgentRunner) RunStep(ctx context.Context, req StepRequest) (result plan.StepResult, runErr error) {
	candidate, ok := a.find(ctx, req.Agent)
	if !ok {
		return plan.StepResult{}, fmt.Errorf("agent %q is no longer in the roster", req.Agent)
	}
	at := harness.Placement{Node: candidate.Node, Harness: candidate.Harness}

	var instructions string
	var servers []acp.MCPServer
	if a.caps != nil {
		var err error
		instructions, servers, err = a.caps.Assemble(candidate)
		if err != nil {
			return plan.StepResult{}, fmt.Errorf("assemble capabilities for %s: %w", req.Agent, err)
		}
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	servers = append(servers, req.MCP...)
	session, err := a.sessions.OpenSession(ctx, at, "", req.Workspace, servers)
	if err != nil {
		if req.OpenFailure != nil {
			err = req.OpenFailure(err)
		}
		return plan.StepResult{}, fmt.Errorf("open session on %s: %w", at, err)
	}
	unsettled := false
	defer func() {
		if unsettled || strings.HasPrefix(session.ID(), "ns_") {
			return
		}
		// A step's session is finished with; closing it releases the agent
		// process's session state on whichever machine it lives.
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer closeCancel()
		if err := a.sessions.CloseSession(closeCtx, at, session.ID()); err != nil {
			stopped, ok := session.(interface{ Stopped() bool })
			if !ok || !stopped.Stopped() {
				runErr = errors.Join(runErr, harness.ErrStopUnconfirmed, err)
			}
		}
	}()

	if strings.HasPrefix(session.ID(), "ns_") {
		if req.RecordSession == nil {
			return result, errors.Join(harness.ErrStopUnconfirmed, errors.New("step session persistence is unavailable"))
		}
		if err := req.RecordSession(session.ID()); err != nil {
			key, _ := execution.KeyOf(ctx)
			record := attempt.Record{Spec: attempt.Spec{ID: key.AttemptID, TaskID: req.TaskID, Node: req.Node}, Session: session.ID()}
			return result, retainedStepDetached(record, err)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return plan.StepResult{}, ctxErr
	}
	harness.ApplyPreferences(ctx, session, candidate.Agent.ID, candidate.Agent.Model, candidate.Agent.Options)
	prompt := req.Context.Render()
	if instructions != "" {
		prompt = instructions + "\n\n" + prompt
	}
	var spent stepSpend
	var answer string
	a.mu.Lock()
	ask, askUser := a.ask, a.askUser
	a.mu.Unlock()
	if turn, ok := session.(harness.TurnRunner); ok && strings.HasPrefix(session.ID(), "ns_") {
		answer, _, err = turn.PromptTurn(ctx, prompt, nil, ask, askUser, spent.wrap(a.progress(ctx, req), req.Agent))
	} else {
		answer, _, err = session.Prompt(ctx, prompt, spent.wrap(a.progress(ctx, req), req.Agent))
	}
	if strings.HasPrefix(session.ID(), "ns_") {
		if !acphost.PromptSettled(err) || (ctx.Err() != nil && !errors.Is(execution.CheckExecution(ctx), task.ErrExecutionStopped)) {
			key, _ := execution.KeyOf(ctx)
			record := attempt.Record{Spec: attempt.Spec{ID: key.AttemptID, TaskID: req.TaskID, Node: req.Node}, Session: session.ID()}
			err = agentexec.Blocked(record, "observer", "观察原步骤的节点命令", "原步骤的观察连接中断。", "建议恢复节点后接续同一次执行。", retainedStepDetached(record, errors.Join(err, ctx.Err())))
		}
	} else {
		answer = strings.TrimSpace(answer)
	}
	unsettled = errors.Is(err, harness.ErrStopUnconfirmed)
	return plan.StepResult{
		Answer:   answer,
		Refs:     ParseRefs(answer),
		Findings: parseFindings(answer),
		Usage:    spent.usage(),
	}, err
}

// stepSpend follows a step's progress for its cost and model.
type stepSpend struct {
	mu   sync.Mutex
	last view.Progress
}

func (s *stepSpend) wrap(next func(view.Progress), agentID string) func(view.Progress) {
	return func(p view.Progress) {
		p.Agent = agentID
		s.mu.Lock()
		s.last = p
		s.mu.Unlock()
		next(p)
	}
}

func (s *stepSpend) usage() *plan.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.last.Usage
	return &plan.Usage{Model: s.last.Settings.Model, Input: int64(u.InputTokens), Output: int64(u.OutputTokens), CachedRead: int64(u.CacheReadTokens), CachedWrite: int64(u.CacheWriteTokens), Context: int64(u.ContextTokens), Reported: u.TokensReported()}
}

func (a *AgentRunner) find(ctx context.Context, id string) (roster.Candidate, bool) {
	for _, c := range a.roster.All(ctx) {
		if c.Agent.ID == id {
			return c, true
		}
	}
	return roster.Candidate{}, false
}

// Steps report structured outcomes through two line prefixes rather than
// through free prose. A ref that only exists inside a paragraph cannot be
// handed to the next step, and a finding nobody can parse cannot trigger a
// re-plan — so the contract is small, explicit, and stated in the prompt.
const (
	refPrefix     = "REF:"
	findingPrefix = "FINDING:"
	replanPrefix  = "REPLAN:"
)

// ParseRefs reads the REF: lines out of an answer.
func ParseRefs(answer string) []plan.Ref {
	var out []plan.Ref
	for _, line := range strings.Split(answer, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), refPrefix)
		if !ok {
			continue
		}
		kind, value, found := strings.Cut(strings.TrimSpace(rest), " ")
		if !found {
			continue
		}
		value, note, _ := strings.Cut(strings.TrimSpace(value), " — ")
		out = append(out, plan.Ref{
			Kind: strings.TrimSpace(kind), Value: strings.TrimSpace(value), Note: strings.TrimSpace(note),
		})
	}
	return out
}

// parseFindings reads the two kinds of report line. A FINDING is a fact the
// next steps should know and is carried into their context. A REPLAN is a
// fact that makes the next steps *wrong*: it stops the run and sends the
// plan back to the planner. Real agents report plenty of the first kind —
// "this is not a git repo", "the API is v2" — and treating every one of
// them as the second kind re-plans a working plan for nothing.
func parseFindings(answer string) []plan.Finding {
	var out []plan.Finding
	for _, line := range strings.Split(answer, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, replanPrefix); ok {
			if text := strings.TrimSpace(rest); text != "" {
				out = append(out, plan.Finding{Text: text, Invalidates: []string{"*"}})
			}
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, findingPrefix); ok {
			if text := strings.TrimSpace(rest); text != "" {
				out = append(out, plan.Finding{Text: text})
			}
		}
	}
	return out
}

// ReportingContract is appended to a step's prompt so the two structured
// outputs have a stated shape. It is deliberately short: a long contract gets
// followed less often than a short one.
const ReportingContract = `

## 交回结果时
- 产出了可引用的东西，用一行 ` + "`REF: <kind> <value> — <说明>`" + `（kind 用 git / blob）
- 后面的步骤应该知道的事实（环境、约束、你的判断），用一行 ` + "`FINDING: <一句话>`" + `；它会被带给后续步骤，不会打断计划
- 只有当你发现**后面的步骤本身已经错了、照原计划做下去没有意义**时，才用一行 ` + "`REPLAN: <为什么>`" + `；这会停下整个计划重新规划，代价很大，不要用它报告小事
- 其余正常写。没有就不写，不要编。`

// SetObserver installs where step progress goes; nil discards it.
func (a *AgentRunner) SetObserver(observe func(StepRequest, view.Progress)) { a.observe = observe }

func (a *AgentRunner) progress(ctx context.Context, req StepRequest) func(view.Progress) {
	return func(p view.Progress) {
		agentexec.EmitProgress(ctx, p)
		if a.observe != nil {
			a.observe(req, p)
		}
	}
}

func (a *AgentRunner) SetQuestionHandlers(ask permission.AskFunc, askUser acphost.AskUserFunc) {
	a.mu.Lock()
	a.ask, a.askUser = ask, askUser
	a.mu.Unlock()
}
func (a *AgentRunner) ResumeStep(ctx context.Context, req StepRequest, record attempt.Record, attached func(nodewire.SessionState) error) (plan.StepResult, error) {
	manager, ok := a.sessions.(interface {
		AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
	})
	if !ok {
		return plan.StepResult{}, errors.New("retained step session interface unavailable")
	}
	session, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		return plan.StepResult{}, err
	}
	inspector, ok := session.(harness.RetainedSessionInspector)
	if !ok {
		return plan.StepResult{}, errors.New("retained step evidence unavailable")
	}
	state, err := inspector.InspectRetained(ctx)
	if err != nil {
		return plan.StepResult{}, err
	}
	if err := attached(state); err != nil {
		return plan.StepResult{}, err
	}
	if record.State != attempt.Running {
		return plan.StepResult{}, nil
	}
	a.mu.Lock()
	ask, askUser := a.ask, a.askUser
	a.mu.Unlock()
	var spent stepSpend
	answer, _, err := session.ResumeTurn(ctx, ask, askUser, spent.wrap(a.progress(ctx, req), record.Agent))
	return plan.StepResult{Answer: answer, Refs: ParseRefs(answer), Findings: parseFindings(answer), Usage: spent.usage()}, err
}
func (a *AgentRunner) CloseRetainedStep(ctx context.Context, record attempt.Record) error {
	return a.sessions.CloseSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session)
}

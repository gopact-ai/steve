package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/view"
)

// Sessions opens a session on the planning agent. The harness manager
// satisfies it; the planner needs only this much.
type Sessions interface {
	OpenSession(ctx context.Context, at harness.Placement, upstreamID, workdir string, servers []acp.MCPServer) (harness.Runner, error)
	CloseSession(ctx context.Context, at harness.Placement, upstreamID string) error
}

// LLM plans by asking a model to decompose the goal, and holds it to a typed
// contract: the answer must be a plan that validates, or it is sent back
// with the reason.
//
// Steve never calls a model API itself — it drives ACP agents — so the
// planning model is one of the configured agents, opened in its own session
// with nothing but the goal, the roster and the contract. What comes back is
// data the executor runs; the model is not in the loop after that. That is
// the division the evidence supports: a model is good at decomposing an
// open goal into steps and poor at being a manager of long-lived peers, so
// it gets the first job and not the second.
type LLM struct {
	// Agent is the catalog id of the planning agent, and At is where it
	// runs. Workspaces gives its session the project's directory there.
	Agent      string
	At         harness.Placement
	Workspaces project.Workspaces
	Sessions   Sessions
	// Timeout bounds one planning call. Zero takes the default.
	Timeout time.Duration
	// Attempts is how many times an invalid plan is sent back with the
	// validation error before giving up. Zero takes the default.
	Attempts int
}

const (
	defaultPlanTimeout  = 3 * time.Minute
	defaultPlanAttempts = 2
)

func (l LLM) Name() string { return "llm:" + l.Agent }

func (l LLM) Plan(ctx context.Context, req Request) (plan.Plan, error) {
	if l.Sessions == nil {
		return plan.Plan{}, fmt.Errorf("llm planner has no way to open a session")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = defaultPlanTimeout
	}
	attempts := l.Attempts
	if attempts <= 0 {
		attempts = defaultPlanAttempts
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if l.Workspaces == nil {
		return plan.Plan{}, fmt.Errorf("llm planner has no workspaces to open a session in")
	}
	// The planner looks at the project from its own worktree: it reads,
	// it does not write, and it may sit on any machine.
	workspace, err := l.Workspaces.Materialize(ctx, project.Request{Project: req.ProjectID, Node: l.At.Node, Isolated: true, Owner: "plan-" + req.TaskID})
	if err != nil {
		return plan.Plan{}, fmt.Errorf("planning session workspace: %w", err)
	}
	defer discard(ctx, l.Workspaces, workspace)
	session, err := l.Sessions.OpenSession(ctx, l.At, "", workspace.Path, nil)
	if err != nil {
		return plan.Plan{}, fmt.Errorf("open planning session on %s: %w", l.At, err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer closeCancel()
		if err := l.Sessions.CloseSession(closeCtx, l.At, session.ID()); err != nil {
			session.Abort()
		}
	}()

	prompt := renderPrompt(req)
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			// The contract is enforced, not hoped for: an invalid plan goes
			// back with the exact reason, in the same session, so the model
			// fixes that rather than starting over.
			prompt = "上一份计划无法执行：" + lastErr.Error() + "\n\n只修这个问题，重新输出完整的 JSON。"
		}
		answer, _, err := session.Prompt(ctx, prompt, func(view.Progress) {})
		if err != nil {
			return plan.Plan{}, fmt.Errorf("planning agent %s: %w", l.Agent, err)
		}
		built, err := parsePlan(answer, req, l.Name())
		if err != nil {
			lastErr = err
			continue
		}
		return built, nil
	}
	return plan.Plan{}, fmt.Errorf("planning agent %s produced no valid plan after %d attempts: %w", l.Agent, attempts, lastErr)
}

// wirePlan is the contract the model writes to. It is the plan's own shape
// minus runtime fields, so nothing the model says can set a step's state.
type wirePlan struct {
	Steps []wireStep `json:"steps"`
}

type wireStep struct {
	ID       string      `json:"id"`
	Goal     string      `json:"goal"`
	Needs    []string    `json:"needs,omitempty"`
	Merge    []string    `json:"merge,omitempty"`
	Requires []string    `json:"requires,omitempty"`
	Agent    string      `json:"agent,omitempty"`
	Verify   *wireVerify `json:"verify"`
}

type wireVerify struct {
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Why     string `json:"why,omitempty"`
}

// parsePlan turns the model's answer into a validated plan. It tolerates a
// fenced block and prose around the JSON, and nothing else.
func parsePlan(answer string, req Request, by string) (plan.Plan, error) {
	raw, ok := extractJSON(answer)
	if !ok {
		return plan.Plan{}, fmt.Errorf("the answer contains no JSON object")
	}
	var wire wirePlan
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return plan.Plan{}, fmt.Errorf("the JSON does not match the contract: %v", err)
	}
	built := plan.Plan{
		TaskID: req.TaskID, Goal: req.Goal, By: by,
		Because: firstNonEmpty(req.Trigger, "initial plan"),
	}
	if req.Current.ID != "" {
		built = req.Current
		built.By = by
		built.Because = req.Trigger
	}
	built.Steps = built.Steps[:0]
	done := map[string]plan.Step{}
	for _, s := range req.Current.Steps {
		if s.State == plan.StepDone {
			done[s.ID] = s
		}
	}
	for _, w := range wire.Steps {
		step := plan.Step{
			ID: w.ID, Goal: w.Goal, Needs: w.Needs, Merge: w.Merge,
			Requires: w.Requires, Agent: w.Agent, State: plan.StepPending,
		}
		if w.Verify != nil {
			step.Verify = &plan.Verify{
				Kind: plan.VerifyKind(w.Verify.Kind), Command: w.Verify.Command,
				Agent: w.Verify.Agent, Why: w.Verify.Why,
			}
		}
		// A revision keeps what already finished. The model may reshape
		// what comes next; it does not get to un-finish work.
		if prior, ok := done[w.ID]; ok {
			step.State, step.Result, step.Attempts = prior.State, prior.Result, prior.Attempts
		}
		built.Steps = append(built.Steps, step)
	}
	if err := plan.Validate(built); err != nil {
		return plan.Plan{}, err
	}
	return built, nil
}

// extractJSON finds the outermost object in an answer that may also contain
// prose or a code fence. The model is told to answer with JSON only; this is
// tolerance for the ways that instruction gets bent, not a parser for prose.
func extractJSON(answer string) (string, bool) {
	start := strings.Index(answer, "{")
	end := strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return answer[start : end+1], true
}

// renderPrompt writes the planning brief: the goal, who is available and
// what each can reach, the contract, and — on a revision — what happened.
func renderPrompt(req Request) string {
	var b strings.Builder
	b.WriteString("把下面的目标拆成可以分头执行的步骤，输出一份 JSON 计划。\n\n")
	fmt.Fprintf(&b, "## 目标\n%s\n\n", req.Goal)

	b.WriteString("## 可用的 agent 与机器\n")
	for _, c := range req.Roster {
		where := c.Node
		if where == "" {
			where = "hub"
		}
		if !c.Eligible {
			fmt.Fprintf(&b, "- %s（%s）不可用：%s\n", c.Agent.ID, where, c.Why)
			continue
		}
		fmt.Fprintf(&b, "- %s 在 %s，能力：%s\n", c.Agent.ID, where, strings.Join(c.Capabilities, ", "))
	}
	if req.TurnsLeft > 0 {
		fmt.Fprintf(&b, "\n预算还剩 %d 轮。步骤数不要超过预算。\n", req.TurnsLeft)
	}

	if req.Current.ID != "" {
		b.WriteString("\n## 上一版计划跑到了哪\n")
		for _, s := range req.Current.Steps {
			line := fmt.Sprintf("- %s [%s] %s", s.ID, s.State, s.Goal)
			if s.Result != nil {
				if s.Result.Error != "" {
					line += " — 失败：" + s.Result.Error
				}
				for _, f := range s.Result.Findings {
					line += "\n  发现：" + f.Text
				}
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "\n触发重规划的原因：%s\n", req.Trigger)
		b.WriteString("已完成的步骤保留原 id，会复用结果；只改需要改的。\n")
	}

	b.WriteString(`
## 规则
- 每一步是一个能独立交给一个 agent 做完的目标，写得让一个没有任何上下文的人也能动手。
- 用 requires 说明这一步需要什么能力（从上面列出的能力里选），不要指定 agent，除非非它不可。
- needs 是依赖的步骤 id。可以并行的步骤不要串起来。
- 两个并行分支必须由一个显式的 merge 步骤收敛（merge 列出它收敛的步骤 id），因为一个工作区同一时刻只能有一个写者。同一个依赖只在 needs 或 merge 里出现一次，不要两边都写。
- 每台机器的工作目录只有它自己看得见。一步不能读另一台机器上的文件；跨机器只能靠上一步交回的 REF（提交、分支、摘要）。收敛步骤核对的是各分支交回的结果，不是去找别的机器的路径。
- 每一步都要写 verify：{"kind":"command","command":"…"} 用命令验证；{"kind":"none","why":"…"} 必须给出不验证的理由。
- 步骤越少越好。一个人一步能做完的，不要拆。

## 输出格式
只输出一个 JSON 对象，不要解释，不要 markdown 围栏：
{"steps":[{"id":"build","goal":"…","needs":[],"merge":[],"requires":["gpu"],"agent":"","verify":{"kind":"command","command":"go test ./..."}}]}
`)
	return b.String()
}

// discard drops a worktree if the seam knows how; a seam that cannot has
// nothing to clean.
func discard(ctx context.Context, w project.Workspaces, ws project.Workspace) {
	if d, ok := w.(interface {
		Discard(context.Context, project.Workspace) error
	}); ok && ws.Kind == project.KindWorktree {
		_ = d.Discard(context.WithoutCancel(ctx), ws)
	}
}

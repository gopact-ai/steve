package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/plan"
)

// Executor admits and settles the planning prompt; parsing stays here.
type Executor interface {
	Prompt(context.Context, agentexec.Spec, string, func(string) error) (agentexec.Result, error)
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
	Agent    string
	Executor Executor
	// Timeout bounds one planning call. Zero takes the default.
	Timeout time.Duration
	// Attempts is how many times an invalid plan is sent back with the
	// validation error before giving up. Zero takes the default.
	Attempts int
}

const (
	DefaultTimeout  = 3 * time.Minute
	DefaultAttempts = 2
)

func (l LLM) Name() string { return "llm:" + l.Agent }

func (l LLM) Plan(ctx context.Context, req Request) (plan.Plan, error) {
	if l.Executor == nil {
		return plan.Plan{}, fmt.Errorf("llm planner has no bounded agent executor")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	attempts := l.Attempts
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	brief := renderPrompt(req)
	prompt := brief
	var lastErr error
	var previous string
	for round := range attempts {
		if round > 0 {
			prompt = brief + "\n\n上一份计划无法执行：" + lastErr.Error() + "\n\n上一份输出：\n" + previous + "\n\n只修这个问题，重新输出完整的 JSON。"
		}
		var built plan.Plan
		result, err := l.Executor.Prompt(ctx, agentexec.Spec{TaskID: req.TaskID, TurnID: fmt.Sprintf("plan/%s/r%d/prompt/%d", req.TaskID, req.Current.Rev+1, round+1),
			Agent: l.Agent, Project: req.ProjectID, Kind: attempt.KindPlan, Timeout: timeout}, prompt, func(answer string) error {
			var err error
			built, err = parsePlan(answer, req, l.Name())
			return err
		})
		if err == nil {
			return built, nil
		}
		var invalid *agentexec.ValidationError
		if !errors.As(err, &invalid) {
			return plan.Plan{}, fmt.Errorf("planning agent %s: %w", l.Agent, err)
		}
		lastErr, previous = invalid.Cause, result.Answer
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
	built.Steps = nil
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
	b.WriteString("步骤的 requires 用选择器写：kind:id，如 tool:docker、mcp:github、hardware:gpu、model:claude*、network:internal；裸词是标签。只要求真的需要的。\n")
	for _, c := range req.Roster {
		where := c.Node
		if where == "" {
			where = "hub"
		}
		if !c.Eligible {
			fmt.Fprintf(&b, "- %s（%s）不可用：%s\n", c.Agent.ID, where, c.Why)
			continue
		}
		about := ""
		if c.Agent.About != "" {
			about = "，适合：" + c.Agent.About
		}
		fmt.Fprintf(&b, "- %s 在 %s%s，能力：%s\n", c.Agent.ID, where, about, ability.Compact(c.Snapshot, c.Harness, 8))
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

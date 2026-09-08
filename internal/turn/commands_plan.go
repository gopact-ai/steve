// The /plan, /plans and /fleet commands: running a plan by hand and
// reading how plans and machines stand.

package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"sort"
	"strings"
)

// planCmd takes a goal that needs more than one agent and runs it as a plan.
//
// The whole point of the verb is that the user does not have to know which
// machine does what: they state the goal, and placement is decided against
// the live roster. What they get back is the tree, so the answer is
// inspectable rather than a paragraph claiming success.
func (c commands) planCmd(ctx context.Context, req Request, rest string) (Result, error) {
	ctx = agentexec.WithProgress(ctx, req.OnProgress)
	title := c.text.T(i18n.CardPlan)
	goal := strings.TrimSpace(rest)
	if c.supervisor == nil || c.plans == nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanDisabled)}, nil
	}
	if goal == "" {
		return Result{Title: title, Text: c.text.T(i18n.PlanUsage, protocol.CommandPlan)}, nil
	}
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		var user UserError
		if errors.As(err, &user) {
			return Result{Title: title, Text: user.Text}, nil
		}
		return Result{Title: title, Text: c.text.T(i18n.PlanFailed, err)}, nil
	}
	tracked, err := c.openPreparedPlanTask(ctx, req, goal, binding.ProjectID)
	if err != nil {
		return Result{Title: title, Text: err.Error()}, nil
	}
	if c.executions != nil {
		scope, err := c.executions.Begin(ctx, execution.Key{TaskID: tracked.ID, InstanceID: "plan/" + tracked.ID})
		if err != nil {
			return Result{Title: title, Text: err.Error()}, nil
		}
		defer scope.Finish(nil)
		ctx = scope.Context()
	}
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	var candidates []roster.Candidate
	if c.fleet != nil {
		candidates = c.fleet.All(ctx)
	}
	var proposed plan.Plan
	if tracked.PreparedPlan != nil {
		proposed, err = preparedTaskPlan(tracked)
	} else {
		proposed, err = c.supervisor.Plan(ctx, planner.Request{Goal: goal, TaskID: tracked.ID, ProjectID: tracked.ProjectID, Roster: candidates, TurnsLeft: tracked.Budget.MaxTurns - tracked.Budget.Turns})
	}
	if err != nil {
		if blocked := planRecoveryError(err); blocked != nil {
			return Result{Title: title}, blocked
		}
		return Result{Title: title, Text: c.text.T(i18n.PlanFailed, err)}, nil
	}
	// The supervisor records its run before taking the base snapshot. This
	// leaves a recoverable owner even if snapshotting or startup is interrupted.
	proposed.ProjectID, proposed.Execution = tracked.ProjectID, execution.Token(ctx)
	stored, err := c.plans.Create(proposed)
	if err != nil {
		return Result{Title: title}, retainedBlocked("plan-store", "保存已生成的计划", "规划结果尚未保存到任务。", err.Error(), "建议恢复存储后重新核对已提交的规划结果。", err)
	}
	outcome, runErr := c.supervisor.Execute(ctx, stored)
	if runErr == nil {
		runErr = ctx.Err()
	}
	return c.planExecutionResult(ctx, stored.ID, outcome, runErr)
}

// plansCmd lists what has been planned in this conversation, newest first.
func (c commands) plansCmd(req Request, rest string) Result {
	title := c.text.T(i18n.CardPlan)
	if c.plans == nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanDisabled)}
	}
	id := strings.TrimSpace(strings.TrimPrefix(rest, "#"))
	if id != "" {
		stored, ok := c.plans.Latest(id)
		if !ok {
			return Result{Title: title, Text: c.text.T(i18n.PlanUnknown, id)}
		}
		var b strings.Builder
		b.WriteString(c.planTree(stored, exec.Outcome{}))
		// The revisions are the interesting part: what changed, and why.
		if revisions := c.plans.Revisions(id); len(revisions) > 1 {
			fmt.Fprintf(&b, "\n\n**%s**", c.text.T(i18n.PlanRevisions))
			for _, rev := range revisions {
				fmt.Fprintf(&b, "\nrev %d · %s · %s", rev.Rev, rev.By, rev.Because)
			}
		}
		return Result{Title: title, Text: b.String()}
	}
	all := c.plans.List()
	if len(all) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.PlansEmpty, protocol.CommandPlan)}
	}
	var b strings.Builder
	for i, p := range all {
		if i >= 8 {
			fmt.Fprintf(&b, "\n… %d more", len(all)-8)
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		done := 0
		for _, s := range p.Steps {
			if s.Result != nil && s.Result.Error == "" {
				done++
			}
		}
		fmt.Fprintf(&b, "**%s** rev %d · %d/%d · %s\n%s",
			p.ID, p.Rev, done, len(p.Steps), p.By, p.Goal)
	}
	return Result{Title: title, Text: b.String()}
}

// fleetCmd answers "what can I actually reach right now". It reads the live
// roster, so a node that died a minute ago says so here rather than at the
// moment someone needed it.
func (c commands) fleetCmd(ctx context.Context, req Request) Result {
	title := c.text.T(i18n.CardFleet)
	if c.fleet == nil {
		return Result{Title: title, Text: c.text.T(i18n.FleetLocal)}
	}
	candidates := c.fleet.All(ctx)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Agent.ID < candidates[j].Agent.ID })
	var b strings.Builder
	for i, item := range candidates {
		if i > 0 {
			b.WriteString("\n")
		}
		mark := "✓"
		if !item.Eligible {
			mark = "✗"
		}
		fmt.Fprintf(&b, "%s **%s** · %s · %s", mark, item.Agent.ID, nodewire.Place(item.Node), item.Harness)
		if len(item.Capabilities) > 0 {
			fmt.Fprintf(&b, "\n%s", strings.Join(item.Capabilities, ", "))
		}
		if item.Why != "" {
			fmt.Fprintf(&b, "\n> %s", item.Why)
			b.WriteString(c.repairHint(ctx, item))
		}
	}
	return Result{Title: title, Text: b.String()}
}

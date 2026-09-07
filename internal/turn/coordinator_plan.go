package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

// Supervisor is the plan side of the coordinator: it decomposes a goal,
// places each step on a machine that can run it, and drives the whole thing.
// Without one wired, /plan says so rather than pretending to have planned.
type Supervisor interface {
	Plan(ctx context.Context, req planner.Request) (plan.Plan, error)
	Execute(ctx context.Context, p plan.Plan) (exec.Outcome, error)
	Name() string
	PrepareRecovery(ctx context.Context) error
	OpenRuns(ctx context.Context) ([]exec.RunRecord, error)
	Resume(ctx context.Context, rec exec.RunRecord) (exec.Outcome, error)
}

// SetSupervisor enables the planning verbs.
func (c *Coordinator) SetSupervisor(s Supervisor, plans *plan.Store, fleet *roster.Roster) {
	c.supervisor = s
	c.plans = plans
	c.fleet = fleet
}

// planCmd takes a goal that needs more than one agent and runs it as a plan.
//
// The whole point of the verb is that the user does not have to know which
// machine does what: they state the goal, and placement is decided against
// the live roster. What they get back is the tree, so the answer is
// inspectable rather than a paragraph claiming success.
func (c *Coordinator) planCmd(ctx context.Context, req Request, rest string) (Result, error) {
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

func (c *Coordinator) planExecutionResult(ctx context.Context, planID string, outcome exec.Outcome, runErr error) (Result, error) {
	title := c.text.T(i18n.CardPlan)
	if blocked := planRecoveryError(runErr); blocked != nil {
		return Result{Title: title}, blocked
	}
	if runErr != nil {
		runs, lookupErr := c.supervisor.OpenRuns(ctx)
		if lookupErr != nil {
			return Result{Title: title}, retainedBlocked("plan-state", "读取计划的执行状态", "暂时无法确认计划是否已经完成。", lookupErr.Error(), "建议恢复存储后重新检查原计划。", errors.Join(runErr, lookupErr))
		}
		for _, run := range runs {
			if run.PlanID == planID {
				return Result{Title: title}, retainedBlocked("plan-state", "读取原计划的持久执行阶段", "计划尚未完整完成。", runErr.Error(), "建议恢复执行条件后重新检查，保留已完成步骤。", runErr)
			}
		}
	}
	final, _ := c.plans.Latest(planID)
	if runErr != nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanStopped, planID, runErr) + "\n\n" + c.planTree(final, outcome)}, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.PlanDone, planID, len(final.Steps)) + "\n\n" + c.planTree(final, outcome) + landingSummary(outcome)}, nil
}

// openPlanTask gives the plan a task so its budget, anchor and history are
// the same machinery every other kind of work uses.
func (c *Coordinator) openPlanTask(req Request, goal, projectID string) (task.Task, error) {
	return c.openPlanTaskWithPrepared(req, goal, projectID, nil)
}

func (c *Coordinator) openPreparedPlanTask(ctx context.Context, req Request, goal, projectID string) (task.Task, error) {
	if c.tasks == nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanDisabled))
	}
	var prepared *task.PreparedPlan
	if pure, ok := c.supervisor.(interface {
		PrepareRulePlan(context.Context, string, string) (plan.Plan, bool, error)
	}); ok {
		built, available, err := pure.PrepareRulePlan(ctx, goal, projectID)
		if err != nil {
			return task.Task{}, err
		}
		if available {
			if built.By != "rule" || built.ID != "" || built.TaskID != "" || built.Execution != nil || built.Goal != goal || built.ProjectID != projectID {
				return task.Task{}, errors.New("pure planning result has inconsistent ownership")
			}
			raw, err := json.Marshal(built)
			if err != nil {
				return task.Task{}, err
			}
			prepared = &task.PreparedPlan{Snapshot: raw}
		}
	}
	return c.openPlanTaskWithPrepared(req, goal, projectID, prepared)
}

func (c *Coordinator) openPlanTaskWithPrepared(req Request, goal, projectID string, prepared *task.PreparedPlan) (task.Task, error) {
	if c.tasks == nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanDisabled))
	}
	created, err := c.tasks.Create(task.Task{
		Goal: goal, Requester: req.SenderOpenID, Channel: req.ConversationID,
		Node: c.node, Origin: "plan", ProjectID: projectID,
		PreparedPlan: prepared,
	})
	if err != nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanFailed, err))
	}
	if req.MessageID != "" {
		if err := c.tasks.SetAnchor(created.ID, req.ChatID, req.MessageID, string(req.ChatType), req.CardID); err != nil {
			return task.Task{}, fmt.Errorf("persist plan task anchor: %w", err)
		}
	}
	return created, nil
}

// planTree renders the plan as its steps, with where each ran. The tree is
// the answer: a plan that only reports "done" cannot be checked.
func (c *Coordinator) planTree(p plan.Plan, outcome exec.Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**plan %s** rev %d · %s", p.ID, p.Rev, p.By)
	if outcome.Recoveries > 0 {
		fmt.Fprintf(&b, " · %s", c.text.T(i18n.PlanRecovered, outcome.Recoveries))
	}
	b.WriteString("\n")
	answers := map[string]string{}
	for _, r := range outcome.Output.Results {
		answers[r.StepID] = r.Result.Answer
	}
	for _, s := range p.Steps {
		where := "—"
		if s.Result != nil && s.Result.Node != "" {
			where = s.Result.Node
		} else if s.Result != nil {
			where = nodewire.Place("")
		}
		agent := s.Agent
		if s.Result != nil && s.Result.Agent != "" {
			agent = s.Result.Agent
		}
		fmt.Fprintf(&b, "\n%s **%s** · %s@%s", stepMark(s), s.ID, orDash(agent), where)
		if len(s.Requires) > 0 {
			fmt.Fprintf(&b, " · %s", strings.Join(s.Requires, ","))
		}
		fmt.Fprintf(&b, "\n%s", s.Goal)
		if s.Result != nil && s.Result.Error != "" {
			fmt.Fprintf(&b, "\n> %s", s.Result.Error)
		}
	}
	return b.String()
}

func stepMark(s plan.Step) string {
	if s.Result == nil {
		return "·"
	}
	if s.Result.Error != "" {
		return "✗"
	}
	return "✓"
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// plansCmd lists what has been planned in this conversation, newest first.
func (c *Coordinator) plansCmd(req Request, rest string) Result {
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
func (c *Coordinator) fleetCmd(ctx context.Context, req Request) Result {
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

// planTimeout bounds one /plan invocation. A plan runs many steps across
// machines, so it gets more room than a single turn — but not unbounded
// room, because the chat is waiting on it.
const planTimeout = 30 * time.Minute

func landingSummary(outcome exec.Outcome) string {
	var lines []string
	for _, land := range outcome.Landings {
		text := fmt.Sprintf("↳ %s → %s: %s (%d paths)", land.Artifact, land.Project, land.State, len(land.Paths))
		if land.Error != "" {
			text += ": " + land.Error
		}
		lines = append(lines, text)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n")
}

// ResumePlans picks up every plan run a previous process left open: the
// run continues from its checkpoint, the steps in flight take over their
// abandoned attempts, and the outcome reaches the chat through the task's
// anchor like a turn that finished late.
func (c *Coordinator) ResumePlans(ctx context.Context) {
	if c.supervisor == nil || c.plans == nil || c.tasks == nil {
		return
	}
	if err := c.supervisor.PrepareRecovery(ctx); err != nil {
		log.Printf("turn: prepare plan recovery: %v", err)
		return
	}
	open, err := c.supervisor.OpenRuns(ctx)
	if err != nil {
		log.Printf("turn: list open plan runs: %v", err)
		return
	}
	for _, rec := range open {
		tracked, ok := c.tasks.Get(rec.TaskID)
		if ok && c.planRecoveryOwner != nil && c.planRecoveryOwner(tracked) {
			continue
		}
		if !ok || tracked.State == task.StatePaused || tracked.State == task.StateCancelled {
			log.Printf("turn: plan %s run not resumed: task #%s is %s", rec.PlanID, rec.TaskID, tracked.State)
			continue
		}
		go c.resumePlan(ctx, rec, tracked)
	}
}

func (c *Coordinator) resumePlan(ctx context.Context, rec exec.RunRecord, tracked task.Task) {
	log.Printf("turn: resuming plan %s (run %s) for task #%s", rec.PlanID, rec.RunID, tracked.ID)
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	outcome, runErr := c.supervisor.Resume(ctx, rec)
	if runErr == nil {
		runErr = ctx.Err()
	}
	final, _ := c.plans.Latest(rec.PlanID)
	var text string
	if runErr != nil {
		text = c.text.T(i18n.PlanStopped, rec.PlanID, runErr) + "\n\n" + c.planTree(final, outcome)
	} else {
		text = c.text.T(i18n.PlanDone, rec.PlanID, len(final.Steps)) + "\n\n" + c.planTree(final, outcome) + landingSummary(outcome)
	}
	if c.notifier != nil && tracked.AnchorMessage != "" {
		c.notifier(TaskNotice{TaskID: tracked.ID, ChatID: tracked.ChatID, MessageID: tracked.AnchorMessage, Requester: tracked.Requester, Conversation: tracked.Channel, Text: text})
	}
}

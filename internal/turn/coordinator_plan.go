package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
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

// rulePlanner is a supervisor that can draft a plan from rules alone,
// without an agent, so the task can be opened with the plan attached.
type rulePlanner interface {
	PrepareRulePlan(ctx context.Context, goal, projectID string) (plan.Plan, bool, error)
}

// retainedRunReader is a supervisor that can list the plan runs a previous
// process left in flight.
type retainedRunReader interface {
	RetainedRuns(ctx context.Context) ([]exec.RunRecord, error)
}

// retainedPlanner is a supervisor that can pick planning back up for a
// task whose planning attempt was interrupted.
type retainedPlanner interface {
	ResumePlanning(ctx context.Context, taskID string) (plan.Plan, error)
}

// SetSupervisor enables the planning verbs.
func (c *Coordinator) SetSupervisor(s Supervisor, plans *plan.Store, fleet *roster.Roster) {
	c.supervisor = s
	c.plans = plans
	c.fleet = fleet
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
	return Result{Title: title, Text: c.text.T(i18n.PlanDone, planID, len(final.Steps)) + "\n\n" + c.planTree(final, outcome) + c.landingSummary(outcome)}, nil
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
	if pure, ok := c.supervisor.(rulePlanner); ok {
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
		Transport: req.Channel, ChatID: req.ChatID, AnchorMessage: req.MessageID, ChatType: string(req.ChatType), OpenCard: req.CardID,
		Goal: goal, Requester: req.SenderOpenID, Channel: req.ConversationID,
		Node: c.node, Origin: "plan", ProjectID: projectID,
		PreparedPlan: prepared,
	})
	if err != nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanFailed, err))
	}
	return created, nil
}

// cancelOrphanedPlans closes background plan tasks that a restart left
// open. A plan started without a conversation — an automatic conflict
// resolution, say — has no anchor to reply at, so plan recovery passes
// over it, and `/tasks` scopes ids to the chat that owns them. With no
// run left to resume, nothing would ever close it and the board would go
// on counting it as work in flight. resuming holds the tasks whose run is
// about to be picked back up, which are not orphans.
func (c *Coordinator) cancelOrphanedPlans(resuming map[string]bool) {
	if c.tasks == nil {
		return
	}
	for _, tracked := range c.tasks.List("") {
		if tracked.Origin != "plan" || tracked.Channel != "" || resuming[tracked.ID] {
			continue
		}
		// A failure and a pause are both already settled states someone
		// can act on; only work still claiming to run is stranded.
		if tracked.State.Terminal() || tracked.State == task.StateFailed || tracked.State == task.StatePaused {
			continue
		}
		if _, err := c.tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
			slog.Error(fmt.Sprintf("turn: cancel stranded plan task #%s: %v", tracked.ID, err), "task", tracked.ID)
			continue
		}
		slog.Warn(fmt.Sprintf("turn: cancelled plan task #%s left open with no run and no conversation: %s", tracked.ID, tracked.Goal), "task", tracked.ID)
	}
}

// closePlanTask ends the task a plan ran under, with the state the run
// earned: done when it finished, failed when it stopped. Without it the
// task keeps claiming to run for good — a background plan is out of reach
// of both recovery and the task commands, and a foreground one would read
// as running long after its card said it stopped. A task that is already
// terminal is left alone: a step that closed it had the better answer.
func (c *Coordinator) closePlanTask(id string, runErr error) {
	if c.tasks == nil || id == "" {
		return
	}
	if tracked, ok := c.tasks.Get(id); !ok || tracked.State.Terminal() || tracked.State == task.StateFailed {
		return
	}
	to := task.StateDone
	if runErr != nil {
		to = task.StateFailed
	}
	if _, err := c.tasks.Advance(id, to); err != nil {
		slog.Error(fmt.Sprintf("turn: close plan task #%s: %v", id, err), "task", id)
		return
	}
	if runErr != nil {
		slog.Warn(fmt.Sprintf("turn: plan task #%s failed: %v", id, runErr), "task", id)
	}
}

// finishPlanTask closes a plan task that finished, under the execution
// authority the run itself holds. The plan machinery closes the task
// already when the last step lands, and advancing a second time would
// only log an error about a move from done to done.
func (c *Coordinator) finishPlanTask(ctx context.Context, id string) {
	if c.tasks == nil || id == "" {
		return
	}
	if tracked, ok := c.tasks.Get(id); !ok || tracked.State.Terminal() {
		return
	}
	if _, err := c.advanceExecution(ctx, id, task.StateDone); err != nil {
		slog.Error(fmt.Sprintf("turn: close plan task #%s: %v", id, err), "task", id)
	}
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

// planTimeout bounds one /plan invocation. A plan runs many steps across
// machines, so it gets more room than a single turn — but not unbounded
// room, because the chat is waiting on it.
const planTimeout = 30 * time.Minute

func (c *Coordinator) landingSummary(outcome exec.Outcome) string {
	var lines []string
	for _, land := range outcome.Landings {
		text := fmt.Sprintf("↳ %s → %s: %s (%d paths)", land.Artifact, land.Project, land.State, len(land.Paths))
		if land.Error != "" {
			text += ": " + land.Error
		}
		// A conflict that git kept a tree for is one command away from
		// being fixed; saying so beats reporting a dead end.
		if land.State == artifact.LandMergeConflicted && land.Conflict != "" {
			text += "\n  " + resolveHint(c.text, land.Artifact)
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
		slog.Error(fmt.Sprintf("turn: prepare plan recovery: %v", err))
		return
	}
	open, err := c.supervisor.OpenRuns(ctx)
	if err != nil {
		slog.Error(fmt.Sprintf("turn: list open plan runs: %v", err))
		return
	}
	resuming := make(map[string]bool, len(open))
	for _, rec := range open {
		resuming[rec.TaskID] = true
	}
	c.cancelOrphanedPlans(resuming)
	for _, rec := range open {
		tracked, ok := c.tasks.Get(rec.TaskID)
		if ok && c.planRecoveryOwner != nil && c.planRecoveryOwner(tracked) {
			continue
		}
		if !ok || tracked.State == task.StatePaused || tracked.State == task.StateCancelled {
			slog.Warn(fmt.Sprintf("turn: plan %s run not resumed: task #%s is %s", rec.PlanID, rec.TaskID, tracked.State), "plan", rec.PlanID, "run", rec.RunID, "task", rec.TaskID)
			continue
		}
		go c.resumePlan(ctx, rec, tracked)
	}
}

func (c *Coordinator) resumePlan(ctx context.Context, rec exec.RunRecord, tracked task.Task) {
	slog.Info(fmt.Sprintf("turn: resuming plan %s (run %s) for task #%s", rec.PlanID, rec.RunID, tracked.ID), "plan", rec.PlanID, "run", rec.RunID, "task", tracked.ID)
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	outcome, runErr := c.supervisor.Resume(ctx, rec)
	if runErr == nil {
		runErr = ctx.Err()
	}
	// A resumed run is the end of the plan either way. Nothing downstream
	// closes the task here the way the original command would have, and a
	// plan started in the background has no anchor to say so at, so it
	// would sit in the listing as running until the ledger was edited.
	c.closePlanTask(tracked.ID, runErr)
	final, _ := c.plans.Latest(rec.PlanID)
	var text string
	if runErr != nil {
		text = c.text.T(i18n.PlanStopped, rec.PlanID, runErr) + "\n\n" + c.planTree(final, outcome)
	} else {
		text = c.text.T(i18n.PlanDone, rec.PlanID, len(final.Steps)) + "\n\n" + c.planTree(final, outcome) + c.landingSummary(outcome)
	}
	if c.notifier != nil && tracked.AnchorMessage != "" {
		c.notifier(TaskNotice{TaskID: tracked.ID, Transport: tracked.Transport, ChatID: tracked.ChatID, MessageID: tracked.AnchorMessage, Requester: tracked.Requester, Conversation: tracked.Channel, Text: text})
	}
}

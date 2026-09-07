package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log"
	"sort"
	"strings"
	"time"

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
func (c *Coordinator) planCmd(ctx context.Context, req Request, rest string) Result {
	title := c.text.T(i18n.CardPlan)
	goal := strings.TrimSpace(rest)
	if c.supervisor == nil || c.plans == nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanDisabled)}
	}
	if goal == "" {
		return Result{Title: title, Text: c.text.T(i18n.PlanUsage, protocol.CommandPlan)}
	}

	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		var user UserError
		if errors.As(err, &user) {
			return Result{Title: title, Text: user.Text}
		}
		return Result{Title: title, Text: c.text.T(i18n.PlanFailed, err)}
	}
	tracked, err := c.openPlanTask(req, goal, binding.ProjectID)
	if err != nil {
		return Result{Title: title, Text: err.Error()}
	}
	if c.executions != nil {
		scope, err := c.executions.Begin(ctx, execution.Key{TaskID: tracked.ID, InstanceID: "plan/" + tracked.ID})
		if err != nil {
			return Result{Title: title, Text: err.Error()}
		}
		defer scope.Finish(nil)
		ctx = scope.Context()
	}
	// A plan runs many steps across machines, so it gets more room than a
	// single turn — but not unbounded room: the chat is waiting on it.
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()

	var candidates []roster.Candidate
	if c.fleet != nil {
		candidates = c.fleet.All(ctx)
	}
	proposed, err := c.supervisor.Plan(ctx, planner.Request{
		Goal: goal, TaskID: tracked.ID, ProjectID: tracked.ProjectID, Roster: candidates,
		TurnsLeft: tracked.Budget.MaxTurns - tracked.Budget.Turns,
	})
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanFailed, err)}
	}
	// The plan is about the task's project, whatever the planner said, and
	// every step starts from where the project is right now.
	proposed.ProjectID = tracked.ProjectID
	if c.artifacts != nil {
		if p, ok, perr := c.projects.Get(ctx, tracked.ProjectID); perr == nil && ok {
			base, _, serr := c.artifacts.SnapshotCanonical(ctx, p, c.artifacts.CanonicalOf(ctx, p.ID), "plan", "base of plan for task #"+tracked.ID)
			if serr != nil {
				return Result{Title: title, Text: c.text.T(i18n.PlanFailed, serr)}
			}
			proposed.Base = base.ID
		}
	}
	stored, err := c.plans.Create(proposed)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.PlanFailed, err)}
	}

	outcome, runErr := c.supervisor.Execute(ctx, stored)
	if runErr == nil {
		runErr = ctx.Err()
	}
	final, _ := c.plans.Latest(stored.ID)
	if runErr != nil {
		// A failed plan is a result, not an absence of one: the tree shows
		// how far it got and which step stopped it.
		return Result{
			Title: title,
			Text: c.text.T(i18n.PlanStopped, stored.ID, runErr) + "\n\n" +
				c.planTree(final, outcome),
		}
	}
	return Result{
		Title: title,
		Text:  c.text.T(i18n.PlanDone, stored.ID, len(final.Steps)) + "\n\n" + c.planTree(final, outcome) + landingSummary(outcome),
	}
}

// openPlanTask gives the plan a task so its budget, anchor and history are
// the same machinery every other kind of work uses.
func (c *Coordinator) openPlanTask(req Request, goal, projectID string) (task.Task, error) {
	if c.tasks == nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanDisabled))
	}
	created, err := c.tasks.Create(task.Task{
		Goal: goal, Requester: req.SenderOpenID, Channel: req.ConversationID,
		Node: c.node, Origin: "plan", ProjectID: projectID,
	})
	if err != nil {
		return task.Task{}, fmt.Errorf("%s", c.text.T(i18n.PlanFailed, err))
	}
	if req.MessageID != "" {
		if err := c.tasks.SetAnchor(created.ID, req.ChatID, req.MessageID, string(req.ChatType), req.CardID); err != nil {
			log.Printf("turn: anchor plan task %s: %v", created.ID, err)
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

package turn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// goalLimit keeps a task listing readable. The full prompt is not lost: it
// belongs to the archive, which records every turn verbatim.
const goalLimit = 120

// beginTask opens or continues the member's task on this channel and charges a
// turn to it. It returns an empty id when task tracking is disabled, and a
// UserError when the budget is spent — that error is the brake, so it has to
// reach the user rather than be swallowed.
func (c *Coordinator) beginTask(req Request, selected agent.Agent, prompt string) (string, error) {
	if c.tasks == nil {
		return "", nil
	}
	// An onboarding turn runs under a synthetic conversation that is
	// relocated afterwards; a task opened there would be orphaned as
	// forever-running, because task channels do not follow the relocation.
	if strings.HasPrefix(req.ConversationID, onboard.PendingPrefix) {
		return "", nil
	}
	tracked, ok := c.tasks.Active(req.ConversationID, selected.ID)
	if !ok {
		created, err := c.tasks.Create(task.Task{
			Goal:      goal(prompt),
			Requester: req.SenderOpenID,
			Channel:   req.ConversationID,
			Member:    selected.ID,
			Node:      c.node,
			Workspace: selected.Workspace,
		})
		if err != nil {
			// Losing the task record must not cost the user their turn.
			log.Printf("turn: create task: %v", err)
			return "", nil
		}
		tracked = created
	}
	if _, err := c.tasks.Begin(tracked.ID, selected.ID, c.node, ""); err != nil {
		if text, spent := c.budgetStop(tracked); spent {
			return "", UserError{Text: text}
		}
		log.Printf("turn: begin task %s: %v", tracked.ID, err)
		return "", nil
	}
	// The anchor is what a restarted gateway replies to when it resumes
	// this task; refresh it every turn so delivery lands by the newest
	// exchange (and inside the right topic).
	if req.MessageID != "" {
		if err := c.tasks.SetAnchor(tracked.ID, req.ChatID, req.MessageID, string(req.ChatType), req.CardID); err != nil {
			log.Printf("turn: anchor task %s: %v", tracked.ID, err)
		}
	}
	return tracked.ID, nil
}

// finishTask closes the attempt. Tokens stay zero for now: usage rides on the
// ACP prompt response, which the runner does not surface yet.
func (c *Coordinator) finishTask(id string, turnErr error) {
	if c.tasks == nil || id == "" {
		return
	}
	if _, err := c.tasks.Finish(id, outcome(turnErr), task.Tokens{}, 0); err != nil {
		log.Printf("turn: finish task %s: %v", id, err)
	}
}

// closeTask ends the member's task on a session reset. /new means "start over",
// and a task that survived the session it was attempted through would silently
// keep charging turns against work nobody is doing any more.
func (c *Coordinator) closeTask(conversationID, agentID string) {
	if c.tasks == nil {
		return
	}
	tracked, ok := c.tasks.Active(conversationID, agentID)
	if !ok {
		return
	}
	if _, err := c.tasks.Advance(tracked.ID, task.StateDone); err != nil {
		log.Printf("turn: close task %s: %v", tracked.ID, err)
	}
}

func outcome(err error) task.Outcome {
	switch {
	case err == nil:
		return task.OutcomeOK
	case errors.Is(err, context.DeadlineExceeded):
		return task.OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

func goal(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
	if len([]rune(trimmed)) <= goalLimit {
		return trimmed
	}
	return string([]rune(trimmed)[:goalLimit]) + "…"
}

// budgetStop names the limit that stopped the task. Saying which one it was is
// the difference between a brake the user can act on and one that just says no.
func (c *Coordinator) budgetStop(tracked task.Task) (string, bool) {
	limit, spent := tracked.Budget.Exhausted()
	if !spent {
		return "", false
	}
	if limit == "turns" {
		return c.text.T(i18n.BudgetTurns, tracked.ID, tracked.Budget.MaxTurns, protocol.CommandNew), true
	}
	return c.text.T(i18n.BudgetElapsed, tracked.ID, tracked.Budget.MaxElapsed.Round(time.Minute), protocol.CommandNew), true
}

// taskFields surfaces the live budget on /status. Showing spend beside the
// limit is what turns the brake from a surprise into something the user can
// see coming.
func (c *Coordinator) taskFields(conversationID, agentID string) []view.Field {
	if c.tasks == nil {
		return nil
	}
	tracked, ok := c.tasks.Active(conversationID, agentID)
	if !ok {
		return nil
	}
	return []view.Field{
		{Label: "Task", Value: "#" + tracked.ID, IsMetric: true},
		{Label: "Turns", Value: fmt.Sprintf("%d/%d", tracked.Budget.Turns, tracked.Budget.MaxTurns), IsMetric: true},
		{Label: "Elapsed", Value: fmt.Sprintf("%s/%s",
			tracked.Budget.Elapsed.Round(time.Second), tracked.Budget.MaxElapsed.Round(time.Minute)), IsMetric: true},
		{Label: "Goal", Value: tracked.Goal, Wide: true},
	}
}

// tasksCmd lists what this conversation has been working on. The listing is
// the whole point of a task outliving its turn, so it is a command rather than
// something only the debug API can see.
func (c *Coordinator) tasksCmd(req Request) Result {
	title := c.text.T(i18n.CardTasks)
	if c.tasks == nil {
		return Result{Title: title, Text: c.text.T(i18n.TasksEmpty)}
	}
	all := c.tasks.List(req.ConversationID)
	if len(all) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.TasksEmpty)}
	}
	var b strings.Builder
	for i, tracked := range all {
		if i >= tasksListed {
			fmt.Fprintf(&b, "\n… %d more", len(all)-tasksListed)
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**#%s** %s · %s · %d/%d turns · %s",
			tracked.ID, statusMark(tracked.State), tracked.Member,
			tracked.Budget.Turns, tracked.Budget.MaxTurns,
			tracked.Budget.Elapsed.Round(time.Second))
		if tracked.Goal != "" {
			fmt.Fprintf(&b, "\n%s", tracked.Goal)
		}
	}
	return Result{Title: title, Text: b.String()}
}

// tasksListed keeps the card inside its byte budget; the rest are a count.
const tasksListed = 8

func statusMark(state task.State) string {
	switch state {
	case task.StateRunning:
		return "running"
	case task.StateBlocked:
		return "blocked"
	case task.StateReview:
		return "review"
	case task.StateDone:
		return "done"
	case task.StateFailed:
		return "failed"
	default:
		return string(state)
	}
}

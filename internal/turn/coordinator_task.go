package turn

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
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

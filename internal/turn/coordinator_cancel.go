// Stop is one gesture for everything the user's task set in motion. The
// running turn is cancelled; the children the task delegated are stopped
// with it, since they only ever ran on the task's behalf; and the task is
// held, so that nothing a child leaves can start a turn before the user
// speaks again. What that next turn is told about the children comes
// from the delegation service through SetTurnPreface — turn does not
// import delegate.

package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// Preface is what a task's next turn is told before the user's message,
// and how to record that it was told. Told runs once the prompt has
// settled with the agent; a turn that never reached the agent leaves the
// account to the next one, like a delivery without its receipt.
type Preface struct {
	Text string
	Told func()
}

// SetTurnPreface wires the delegation service's account of the children
// that ended, or were stopped, since the task last heard. An empty Text
// adds nothing to the prompt.
func (c *Coordinator) SetTurnPreface(fn func(ctx context.Context, taskID string) Preface) {
	c.turnPreface = fn
}

func (c *Coordinator) cancel(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	// The hold comes first: the turn cancelled next ends by delivering
	// what its children left, and a child stopped after that ends the same
	// way — neither may wake the task.
	covered := c.coveredTasks(conversationID, selected.ID)
	c.holdDelegating(covered, nil)
	running := c.turnInFlight(conversationID, selected.ID)
	result, turnErr := c.cancelTurn(ctx, conversationID, selected)
	stopped, stopErr := c.stopDelegations(ctx, covered)
	// Stamped again now that the children are stopped: a turn composed
	// while that was under way accounted for less than this stop, and its
	// settling must not lift it.
	c.holdDelegating(covered, stopped)
	if !running && len(stopped) > 0 {
		// The reply invites the next message; the window armed against a
		// turn that was just starting must not swallow it.
		c.clearPendingCancel(sessionKey(conversationID, selected.ID))
	}
	names := make([]string, 0, len(stopped))
	for _, child := range stopped {
		names = append(names, fmt.Sprintf("#%s %s@%s", child.ID, child.Member, nodewire.Name(child.Node)))
	}
	var lines []string
	if turnErr == nil && (running || len(stopped) == 0) {
		lines = append(lines, result.Text)
	}
	if len(stopped) > 0 {
		lines = append(lines, c.text.T(i18n.CancelStoppedChildren, strings.Join(names, c.text.T(i18n.ListSeparator))))
	}
	if stopErr != nil {
		lines = append(lines, fmt.Sprintf("stop recorded for %s, execution has not confirmed stopping: %v", strings.Join(names, ", "), stopErr))
	}
	return Result{AgentID: selected.ID, Text: strings.Join(lines, "\n")}, errors.Join(turnErr, stopErr)
}

// coveredTasks is what a stop in this conversation reaches: every task
// the agent holds there, whatever started it — the same scope as the turn
// it cancels, which runs for any of them.
func (c *Coordinator) coveredTasks(conversationID, agentID string) []task.Task {
	if c.tasks == nil {
		return nil
	}
	return c.tasks.Holding(conversationID, agentID)
}

// atWork says a delegated child is still running on its parent's behalf.
func atWork(child task.Task) bool {
	return child.Delegated() && child.State.Holds() && !child.Finished()
}

// owing says the task's next turn has a child to account for: one at
// work, or one that ended with a result the task was not given. A child
// stopped without its execution confirming the stop has no result to
// give; it is reported while the stop's hold lasts, and left to the
// recovery flow after that.
func owing(child task.Task) bool {
	if !child.Delegated() {
		return false
	}
	return atWork(child) || child.Undelivered()
}

// holdDelegating stamps a hold on each covered task with a child to
// account for. A task with nothing delegated is not held: its stop is
// what it always was.
func (c *Coordinator) holdDelegating(covered []task.Task, stopped []task.Task) {
	for _, tracked := range covered {
		if c.owed(tracked, stopped) {
			c.hold(tracked)
		}
	}
}

// owed says the task has a child to account for: one owing now, or one
// among those this stop just stopped — reported whether or not its
// execution confirmed the stop.
func (c *Coordinator) owed(tracked task.Task, stopped []task.Task) bool {
	for _, child := range stopped {
		if child.Parent == tracked.ID {
			return true
		}
	}
	for _, child := range c.tasks.Children(tracked.ID) {
		if owing(child) {
			return true
		}
	}
	return false
}

func (c *Coordinator) hold(tracked task.Task) {
	if _, err := c.tasks.Hold(tracked.ID); err != nil {
		slog.Error(fmt.Sprintf("turn: hold task #%s at stop: %v", tracked.ID, err), "task", tracked.ID, "conversation", tracked.Channel, "agent", tracked.Member)
	}
}

// stopDelegations cancels the children of the covered tasks that are at
// work now — each with its own subtree — and waits once for their
// executions to confirm. Read after the turn ended, not before: the agent
// may have delegated while it was being stopped. The parent is held
// before its first child is stopped: a stopped child ends by delivering
// what it left, and that must find the task held.
func (c *Coordinator) stopDelegations(ctx context.Context, covered []task.Task) ([]task.Task, error) {
	var stopped []task.Task
	var ids []string
	var stopErr error
	for _, tracked := range covered {
		held := false
		for _, child := range c.tasks.Children(tracked.ID) {
			if !atWork(child) {
				continue
			}
			if !held {
				c.hold(tracked)
				held = true
			}
			aside, err := c.tasks.SetAside(child.ID, task.StateCancelled)
			if err != nil {
				stopErr = errors.Join(stopErr, err)
				continue
			}
			stopped = append(stopped, child)
			ids = append(ids, aside...)
			slog.Info(fmt.Sprintf("turn: stopped delegated task #%s with task #%s", child.ID, tracked.ID), "task", child.ID, "parent", tracked.ID, "conversation", tracked.Channel, "agent", child.Member, "node", child.Node)
		}
	}
	if len(ids) > 0 && c.executions != nil {
		stopErr = errors.Join(stopErr, c.stopExecutions(ctx, ids, false))
	}
	return stopped, stopErr
}

// preface is what the turn's task is told before the user's message, and
// told is what the turn calls once the agent has it: that records the
// account as given and, with lift, lifts the hold the turn composed
// under — from then on a child ending is delivered as it ends. A hold
// stamped since is a later stop's, and stays. The turn a stop cancelled
// gives its account without lifting: its end is the stop's doing.
func (c *Coordinator) preface(ctx context.Context, taskID string) (text string, told func(lift bool)) {
	if taskID == "" || c.tasks == nil {
		return "", nil
	}
	// The stamp is read before the account is composed: a stop between
	// the two re-stamps, and this turn must not lift what it did not see.
	var seen time.Time
	if tracked, ok := c.tasks.Get(taskID); ok {
		seen = tracked.HeldAt
	}
	var p Preface
	if c.turnPreface != nil {
		p = c.turnPreface(ctx, taskID)
	}
	if p.Text == "" && seen.IsZero() {
		return "", nil
	}
	return p.Text, func(lift bool) {
		if p.Told != nil {
			p.Told()
		}
		if !lift || seen.IsZero() {
			return
		}
		if _, err := c.tasks.ReleaseHold(taskID, seen); err != nil {
			slog.Error(fmt.Sprintf("turn: release hold on task #%s: %v", taskID, err), "task", taskID)
		}
	}
}

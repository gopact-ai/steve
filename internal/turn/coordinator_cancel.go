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

// delegation is a task the stop has to account for, with the children
// still running on its behalf.
type delegation struct {
	task    task.Task
	running []task.Task
}

func (c *Coordinator) cancel(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	held := c.holdDelegations(conversationID, selected.ID)
	running := c.turnInFlight(conversationID, selected.ID)
	result, turnErr := c.cancelTurn(ctx, conversationID, selected)
	stopped, stopErr := c.stopDelegations(ctx, held)
	if turnErr != nil {
		return Result{}, errors.Join(turnErr, stopErr)
	}
	names := make([]string, 0, len(stopped))
	for _, child := range stopped {
		names = append(names, fmt.Sprintf("#%s %s@%s", child.ID, child.Member, nodewire.Name(child.Node)))
	}
	if stopErr != nil {
		return Result{AgentID: selected.ID, Text: fmt.Sprintf("stop recorded for %s, execution has not confirmed stopping: %v", strings.Join(names, ", "), stopErr)}, stopErr
	}
	var lines []string
	if running || len(stopped) == 0 {
		lines = append(lines, result.Text)
	}
	if len(stopped) > 0 {
		lines = append(lines, c.text.T(i18n.CancelStoppedChildren, strings.Join(names, "、")))
	}
	return Result{AgentID: selected.ID, Text: strings.Join(lines, "\n")}, nil
}

// holdDelegations puts every task the agent holds in the conversation on
// hold if a child of it is still running or still owes it a result, and
// says which children have to be stopped. The hold comes first: the turn
// cancelled next ends by delivering what its children left, and a child
// stopped after that ends the same way — neither may wake the task.
func (c *Coordinator) holdDelegations(conversationID, agentID string) []delegation {
	if c.tasks == nil {
		return nil
	}
	var held []delegation
	for _, tracked := range c.tasks.Holding(conversationID, agentID) {
		var running []task.Task
		owed := false
		for _, child := range c.tasks.Children(tracked.ID) {
			if !child.Delegated() {
				continue
			}
			if child.State.Holds() {
				running = append(running, child)
			} else if child.Undelivered() {
				owed = true
			}
		}
		if len(running) == 0 && !owed {
			continue
		}
		if _, err := c.tasks.Hold(tracked.ID); err != nil {
			slog.Error(fmt.Sprintf("turn: hold task #%s at stop: %v", tracked.ID, err), "task", tracked.ID, "conversation", conversationID, "agent", agentID)
			continue
		}
		held = append(held, delegation{task: tracked, running: running})
	}
	return held
}

// stopDelegations cancels the running children of the held tasks — each
// with its own subtree — and waits for their executions to confirm.
func (c *Coordinator) stopDelegations(ctx context.Context, held []delegation) ([]task.Task, error) {
	var stopped []task.Task
	var stopErr error
	for _, d := range held {
		for _, child := range d.running {
			ids, err := c.tasks.SetAside(child.ID, task.StateCancelled)
			if err != nil {
				stopErr = errors.Join(stopErr, err)
				continue
			}
			stopped = append(stopped, child)
			if c.executions != nil {
				stopErr = errors.Join(stopErr, c.stopExecutions(ctx, ids, false))
			}
			slog.Info(fmt.Sprintf("turn: stopped delegated task #%s with task #%s", child.ID, d.task.ID), "task", child.ID, "parent", d.task.ID, "conversation", d.task.Channel, "agent", child.Member, "node", child.Node)
		}
	}
	return stopped, stopErr
}

// preface is what the turn's task is told before the user's message, and
// told is what the turn calls once the agent has it: that lifts the hold —
// from then on a child ending is delivered as it ends — and records the
// account as given. With nothing to tell, the hold lifts at once.
func (c *Coordinator) preface(ctx context.Context, taskID string) (text string, told func()) {
	if taskID == "" || c.tasks == nil {
		return "", nil
	}
	var p Preface
	if c.turnPreface != nil {
		p = c.turnPreface(ctx, taskID)
	}
	release := func() {
		if _, err := c.tasks.ReleaseHold(taskID); err != nil {
			slog.Error(fmt.Sprintf("turn: release hold on task #%s: %v", taskID, err), "task", taskID)
		}
	}
	if p.Text == "" {
		release()
		return "", nil
	}
	return p.Text, func() {
		if p.Told != nil {
			p.Told()
		}
		release()
	}
}

package turn

import (
	"context"
	"fmt"
	"strings"
)

// Callbacks are what a coordinator reaches that the application can build
// only after the coordinator, because each of them holds it, directly or
// through what it serves. Wire hands them over once.
type Callbacks struct {
	// Supervisor drafts and runs plans.
	Supervisor Supervisor
	// WorkspaceAttach gives a project a directory on a machine that has
	// none.
	WorkspaceAttach func(ctx context.Context, projectID, node string) error
	// AgentGate is the messaging MCP server each session gets injected
	// with its own conversation-bound token. It is nil when the server
	// cannot bind its port, and sessions then get no messaging tools.
	AgentGate AgentGate
	// AfterTurn runs once a turn's attempt is closed and its queued
	// landings are done, delivering the results of delegated children that
	// ended while the turn ran. TurnPreface is the account of children that
	// ended, or were stopped, since the task last heard; an empty Text adds
	// nothing to the prompt. Both belong to delegation, which exists only
	// with AgentGate, and are nil without it.
	AfterTurn   func(taskID string)
	TurnPreface func(ctx context.Context, taskID string) Preface
	// Notifier pushes a task notice to the channel the task came from.
	Notifier func(TaskNotice)
	// Resumer durably accepts a dormant input through the task's channel.
	// It must not start Handle: the task owner has not yet granted the
	// input execution authority. ResumeDispatcher wakes an accepted input
	// after the owner CAS and the turn slot's release; a failed wake does
	// not undo acceptance, since recovery reads the same grant.
	Resumer          func(TaskResume) error
	ResumeDispatcher func(TaskResume)
}

// required is every callback Wire refuses to go without. Each is named
// after its Callbacks field.
func (cb Callbacks) required() []dependency {
	return []dependency{
		{"Supervisor", cb.Supervisor == nil}, {"WorkspaceAttach", cb.WorkspaceAttach == nil},
		{"Notifier", cb.Notifier == nil}, {"Resumer", cb.Resumer == nil},
		{"ResumeDispatcher", cb.ResumeDispatcher == nil},
	}
}

// Wire hands the coordinator its callbacks. It must return before anything
// that can reach the coordinator starts: the callbacks are read without a
// lock from then on. It panics when called a second time, or when a
// callback Callbacks.required lists is missing.
func (c *Coordinator) Wire(cb Callbacks) {
	// Supervisor is required, so a coordinator has one once it is wired.
	if c.supervisor != nil {
		panic("turn: coordinator is already wired")
	}
	var missing []string
	for _, callback := range cb.required() {
		if callback.absent {
			missing = append(missing, callback.name)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("turn: missing callbacks: %s", strings.Join(missing, ", ")))
	}
	c.supervisor, c.attach, c.gate = cb.Supervisor, cb.WorkspaceAttach, cb.AgentGate
	c.afterTurn, c.turnPreface = cb.AfterTurn, cb.TurnPreface
	c.notifier, c.resumer, c.resumeDispatcher = cb.Notifier, cb.Resumer, cb.ResumeDispatcher
}

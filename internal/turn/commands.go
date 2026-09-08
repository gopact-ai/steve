// The slash commands a message can carry: everything a turn does instead
// of prompting the agent. commands shares the coordinator state and helpers
// by embedding; the turn executor never calls back into it, so the
// dependency runs one way.

package turn

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/view"
	"strings"
	"time"
)

// commands answers one message's slash command. It is a view over the
// coordinator, built per request so it carries the request's channel and
// locale; the *Cmd methods read coordinator state and call its helpers,
// and nothing on the prompt path calls back into them.
type commands struct{ *Coordinator }

func (c *Coordinator) commands() commands { return commands{c} }

// dispatch answers cmd when it is one the coordinator handles. handled is
// false for a plain prompt, which the caller runs instead.
func (c commands) dispatch(ctx context.Context, req Request, selected agent.Agent, cmd protocol.Command, rest string) (result Result, handled bool, err error) {
	handled = true
	switch cmd {
	case protocol.CommandNew, protocol.CommandClear:
		result, err = c.reset(ctx, req.ConversationID, selected)
	case protocol.CommandStatus:
		result = c.status(req, selected)
	case protocol.CommandCancel:
		result, err = c.cancel(ctx, req.ConversationID, selected)
	case protocol.CommandSkills:
		result, err = c.skillsCmd(ctx, req, selected, rest)
	case protocol.CommandTasks:
		result = c.tasksCmd(ctx, req, rest)
	case protocol.CommandEvery, protocol.CommandAt:
		result = c.scheduleCmd(req, selected, cmd, rest)
	case protocol.CommandSchedules:
		result = c.schedulesCmd(req, rest)
	case protocol.CommandModel:
		result, err = c.modelCmd(ctx, req, selected, rest)
	case protocol.CommandHistory:
		result, err = c.historyCmd(req, selected, rest)
	case protocol.CommandPlan:
		result, err = c.planCmd(ctx, req, rest)
	case protocol.CommandPlans:
		result = c.plansCmd(req, rest)
	case protocol.CommandFleet:
		if strings.TrimSpace(rest) == "probe" {
			result = c.probeCmd(ctx)
		} else {
			result = c.fleetCmd(ctx, req)
		}
	case protocol.CommandRepair:
		result = c.repairCmd(ctx, req, rest)
	case protocol.CommandProject:
		result, err = c.projectCmd(ctx, req, rest)
	case protocol.CommandGrant:
		result, err = c.grantCmd(ctx, req, rest)
	case protocol.CommandApprove, protocol.CommandDeny:
		result, err = c.decideCmd(ctx, req, cmd, rest)
	case protocol.CommandEffects:
		result, err = c.effectsCmd(ctx, req, rest)
	default:
		handled = false
	}
	return result, handled, err
}

func (c commands) reset(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	session := c.store.Conversation(conversationID).Sessions[selected.ID]
	if err := c.runtime.CloseSession(ctx, harness.Placement{Node: session.NodeID, Harness: session.HarnessID}, session.UpstreamID); err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	busy = c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		// A turn started while the session was being closed; its error path
		// owns the state cleanup, so leave the record alone.
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	// Archive rather than delete. The agent session was closed, not deleted,
	// so the record is all that stands between the user and their own
	// history; dropping it would make a cleared conversation unreachable
	// forever.
	if err := c.store.ArchiveSession(conversationID, selected.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return Result{}, err
	}
	c.closeTask(conversationID, selected.ID)
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.Reset, selected.ID), Recover: true}, nil
}

func (c commands) status(req Request, selected agent.Agent) Result {
	session := c.store.Conversation(req.ConversationID).Sessions[selected.ID]
	sid := session.UpstreamID
	if sid == "" {
		sid = "none"
	}
	title := c.text.T(i18n.CardStatus)
	fields := []view.Field{
		{Label: "Agent", Value: selected.ID, IsMetric: true},
	}
	fields = append(fields, c.taskFields(req.ConversationID, selected.ID)...)
	if c.home == nil {
		fields = append(fields,
			view.Field{Label: "Harness", Value: selected.Harness, IsMetric: true},
			view.Field{Label: "Session", Value: sid, Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	if injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) != home.ModeOwner {
		fields = append(fields,
			view.Field{Label: "Mode", Value: "guest", IsMetric: true},
			view.Field{Label: "Harness", Value: selected.Harness, Wide: true},
			view.Field{Label: "Session", Value: sid, Wide: true},
			view.Field{Label: "Home", Value: "guest", Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	snap, err := c.home.Load(home.ModeOwner)
	if err != nil {
		fields = append(fields,
			view.Field{Label: "Mode", Value: "error", IsMetric: true},
			view.Field{Label: "Home", Value: "error", Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	fields = append(fields,
		view.Field{Label: "Mode", Value: "owner", IsMetric: true},
		view.Field{Label: "Harness", Value: selected.Harness, Wide: true},
		view.Field{Label: "Session", Value: sid, Wide: true},
		view.Field{Label: "Home", Value: snap.Path, Wide: true},
		view.Field{
			Label: "Identity",
			Value: "Soul " + fileOK(snap.Soul) + " · User " + fileOK(snap.User) + " · Memory " + memorySize(snap.Memory),
			Wide:  true,
		},
	)
	if skills := c.skillStatusLine(); skills != "" {
		fields = append(fields, view.Field{Label: "Skills", Value: strings.TrimPrefix(skills, "skills="), Wide: true})
	}
	return statusResult(selected.ID, title, fields)
}

func statusResult(agentID, title string, fields []view.Field) Result {
	rows := make([]string, 0, len(fields))
	for _, field := range fields {
		rows = append(rows, "**"+field.Label+"**  "+field.Value)
	}
	return Result{AgentID: agentID, Title: title, Text: strings.Join(rows, "\n"), Fields: fields}
}

func fileOK(body string) string {
	if strings.TrimSpace(body) == "" {
		return "missing"
	}
	return "ok"
}

func memorySize(body string) string {
	n := len([]byte(body))
	if n >= 1024 {
		return fmt.Sprintf("%dkiB", (n+1023)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

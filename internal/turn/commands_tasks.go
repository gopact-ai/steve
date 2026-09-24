// The /tasks command: listing, inspecting, pausing, stopping and picking
// up tracked tasks by hand.

package turn

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"strings"
	"time"
)

// taskVerb is what the user asked to do to a task.
type taskVerb int

// taskVerbs maps chat words to verbs. The Chinese aliases are not a nicety:
// the surface is Chinese by default, and a verb that only answers to English
// would make half of it untypable.
var taskVerbs = map[string]taskVerb{
	"pause": taskPause, "暂停": taskPause,
	"resume": taskResume, "继续": taskResume, "恢复": taskResume,
	"cancel": taskCancel, "取消": taskCancel, "结束": taskCancel,
	"complete": taskComplete, "done": taskComplete, "完成": taskComplete,
	"show": taskShow, "详情": taskShow,
	"handled": taskHandled, "handle": taskHandled, "已处理": taskHandled, "人工处理": taskHandled, "手动处理": taskHandled,
	"ignore": taskIgnored, "ignored": taskIgnored, "忽略": taskIgnored, "无需关注": taskIgnored,
	"reopen": taskReopen, "重开": taskReopen, "重新打开": taskReopen,
}

// parseTaskArgs reads "pause 12", "12 pause", "12" or "pause", so nobody has
// to remember which half comes first. An unrecognised word is a usage error
// rather than a guess: acting on the wrong task is worse than asking again.
func parseTaskArgs(rest string) (taskVerb, string, bool) {
	verb, id := taskList, ""
	for _, field := range strings.Fields(rest) {
		if trimmed := strings.TrimPrefix(field, "#"); isTaskID(trimmed) {
			id = trimmed
			if verb == taskList {
				verb = taskShow
			}
			continue
		}
		found, ok := taskVerbs[strings.ToLower(field)]
		if !ok {
			return taskList, "", false
		}
		verb = found
	}
	return verb, id, true
}

func isTaskID(s string) bool {
	if prefix, tail, ok := strings.Cut(s, "~"); ok {
		if len(prefix) != 13 || prefix[0] != 'h' {
			return false
		}
		for _, r := range prefix[1:] {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
		s = tail
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// tasksCmd is everything the user can do to a task from the chat. Listing was
// never the point on its own: a task that outlives its turn is only useful if
// the person who started it can look inside it, set it down, and pick it back
// up without leaving the conversation.
func (c commands) tasksCmd(ctx context.Context, req Request, rest string) (Result, error) {
	title := c.text.T(i18n.CardTasks)
	verb, id, ok := parseTaskArgs(rest)
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.TasksUsage, protocol.CommandTasks)}, nil
	}
	if verb == taskList {
		return c.tasksList(req, title), nil
	}
	tracked, found := c.taskTarget(req, id, verb)
	if !found {
		if verb == taskComplete || verb == taskResume {
			text := c.text.T(i18n.TaskNone)
			if id != "" {
				text = c.text.T(i18n.TaskUnknown, id)
			}
			return Result{Title: title, Text: text}, UserError{Text: text}
		}
		switch {
		case id != "":
			return Result{Title: title, Text: c.text.T(i18n.TaskUnknown, id)}, nil
		case verb == taskResume:
			return Result{Title: title, Text: c.text.T(i18n.TaskNonePaused, protocol.CommandTasks)}, nil
		default:
			return Result{Title: title, Text: c.text.T(i18n.TaskNone)}, nil
		}
	}
	switch verb {
	case taskShow:
		return Result{Title: title, Text: c.taskDetail(tracked)}, nil
	case taskPause:
		return c.taskSetAside(ctx, title, tracked, task.StatePaused), nil
	case taskCancel:
		return c.taskSetAside(ctx, title, tracked, task.StateCancelled), nil
	case taskResume:
		return c.taskPickUp(ctx, req, title, tracked)
	case taskComplete:
		return c.taskComplete(ctx, req, title, tracked)
	case taskHandled:
		return c.taskSettle(title, tracked, task.SettlementHandled), nil
	case taskIgnored:
		return c.taskSettle(title, tracked, task.SettlementIgnored), nil
	case taskReopen:
		return c.taskSettle(title, tracked, ""), nil
	}
	return Result{Title: title, Text: c.text.T(i18n.TasksUsage, protocol.CommandTasks)}, nil
}

// taskTarget resolves which task the user meant. An explicit id is scoped to
// this conversation on purpose: task ids are short and guessable, and one chat
// must not be able to reach into another's work.
func (c commands) taskTarget(req Request, id string, verb taskVerb) (task.Task, bool) {
	if id != "" {
		tracked, ok := c.tasks.Get(id)
		// Background work may have no conversation to scope. It still
		// belongs to its recorded transport; missing transport is not
		// permission for a console or chat adapter to claim the task.
		if !ok || tracked.Transport != req.Channel || tracked.Channel != "" && tracked.Channel != req.ConversationID {
			return task.Task{}, false
		}
		return tracked, true
	}
	// List is newest first, so the bare verb acts on what the user most
	// plausibly has in mind — the thing they were just talking about.
	var completed task.Task
	for _, candidate := range c.tasks.List(req.ConversationID) {
		if candidate.Transport != req.Channel {
			continue
		}
		if verb == taskComplete {
			if candidate.Parent == "" && candidate.Origin == "" {
				if candidate.State == task.StateRunning || candidate.State == task.StateReview {
					return candidate, true
				}
				if completed.ID == "" && candidate.State == task.StateDone && candidate.CompletedByUser {
					completed = candidate
				}
			}
			continue
		}
		if verb == taskResume {
			if candidate.State == task.StatePaused {
				return candidate, true
			}
			continue
		}
		if verb == taskHandled || verb == taskIgnored {
			if candidate.State == task.StateFailed && !candidate.Settled() {
				return candidate, true
			}
			continue
		}
		if verb == taskReopen {
			if candidate.Settled() {
				return candidate, true
			}
			continue
		}
		// A task its owner has closed by hand is no longer what a bare
		// verb means, the same way a terminal one is not.
		if !candidate.State.Terminal() && !candidate.Settled() {
			return candidate, true
		}
	}
	if completed.ID != "" {
		return completed, true // Preserve a repeated bare completion when no open root remains.
	}
	return task.Task{}, false
}

// taskSetAside pauses or cancels, in that order: move the record first so a
// turn that happens to finish during the stop cannot flip the task back to
// running, then stop the turn that is actually burning time.
func (c commands) taskSetAside(ctx context.Context, title string, tracked task.Task, to task.State) Result {
	result, _ := c.setTaskAside(ctx, title, tracked, to, false)
	return result
}

// taskSettle records what the user decided about a failed task: they dealt
// with it, or it does not matter, or they want it back in front of them. The
// task keeps its state — the record has to keep saying the work failed — so
// this writes the decision beside it rather than moving it somewhere tidier.
func (c commands) taskSettle(title string, tracked task.Task, as task.Settlement) Result {
	settled, err := c.tasks.Settle(tracked.ID, as)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.TaskSettleState, tracked.ID, statusMark(tracked.State))}
	}
	switch as {
	case task.SettlementHandled:
		return Result{Title: title, Text: c.text.T(i18n.TaskHandled, settled.ID, protocol.CommandTasks)}
	case task.SettlementIgnored:
		return Result{Title: title, Text: c.text.T(i18n.TaskIgnored, settled.ID, protocol.CommandTasks)}
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskReopened, settled.ID)}
}

// Reserve the turn slot until re-entry is accepted and the task is running.
// A channel refusal must not unpause the task; an accepted continuation must
// not enter native execution before the task state is durably advanced.
func (c commands) taskPickUp(ctx context.Context, req Request, title string, tracked task.Task) (result Result, err error) {
	result.Title = title
	defer func() {
		if err != nil {
			err = fmt.Errorf("resume task #%s: %w", tracked.ID, err)
			result.Text = err.Error()
		}
	}()
	if tracked.State == task.StateRunning {
		// A repeated click cannot replace an accepted input's grant or submit
		// another prompt to work that is already resumed.
		result.Text = c.taskDetail(tracked)
		return result, nil
	}
	if !tracked.State.CanMoveTo(task.StateRunning) {
		result.Text = c.text.T(i18n.TaskStuck, tracked.ID, statusMark(tracked.State))
		return result, nil
	}
	identity := req.MessageID
	if req.ExchangeID != "" {
		identity = req.ExchangeID
	}
	if identity == "" {
		return result, fmt.Errorf("durable resume requires a stable control input identity")
	}
	id := fmt.Sprintf("task-resume:%x", sha256.Sum256([]byte(req.Channel+"\x00"+req.ConversationID+"\x00"+identity)))
	admission := task.ResumeAdmission{ID: id, TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	if tracked.ResumeGrant.Admission.ID == id {
		// Replaying the control cannot renew a consumed or revoked grant.
		// Its channel input retains the original execution's result.
		result.Text = c.taskDetail(tracked)
		return result, nil
	}
	resume := TaskResume{
		Admission: admission,
		Transport: tracked.Transport, TaskID: tracked.ID, Goal: tracked.Goal, Member: tracked.Member,
		ConversationID: tracked.Channel, ChatID: tracked.ChatID,
		MessageID: tracked.AnchorMessage, Requester: tracked.Requester, ChatType: tracked.ChatType,
	}
	reserved := false
	defer func() {
		if reserved {
			c.clearActive(tracked.Channel, tracked.Member)
		}
	}()
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	if !c.beginTurn(tracked.Channel, tracked.Member, cancel) {
		cancel()
		return result, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	reserved = true
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := c.resumer(resume); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	moved, err := c.tasks.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, admission)
	if err != nil {
		return result, err
	}
	result.Text = c.taskDetail(moved)
	// Clear before waking a consumer. It may synchronously reserve this
	// same slot, but no channel callback is permission to execute.
	c.clearActive(tracked.Channel, tracked.Member)
	reserved = false
	c.resumeDispatcher(resume)
	return result, nil
}

// tasksList is what this conversation has been working on. The listing is the
// whole point of a task outliving its turn, so it is a command rather than
// something only the debug API can see.
func (c commands) tasksList(req Request, title string) Result {
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
		fmt.Fprintf(&b, "**#%s** %s · %s · %s turns · %s",
			tracked.ID, statusMark(tracked.State), tracked.Member,
			budgetTurns(tracked.Budget),
			tracked.Budget.Elapsed.Round(time.Second))
		if tracked.Goal != "" {
			fmt.Fprintf(&b, "\n%s", tracked.Goal)
		}
	}
	return Result{Title: title, Text: b.String()}
}

// tasksListed keeps the card inside its byte budget; the rest are a count.
const tasksListed = 8

// The /tasks command: listing, inspecting, pausing, stopping and picking
// up tracked tasks by hand.

package turn

import (
	"context"
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
	"show": taskShow, "详情": taskShow,
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
func (c commands) tasksCmd(ctx context.Context, req Request, rest string) Result {
	title := c.text.T(i18n.CardTasks)
	if c.tasks == nil {
		return Result{Title: title, Text: c.text.T(i18n.TasksEmpty)}
	}
	verb, id, ok := parseTaskArgs(rest)
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.TasksUsage, protocol.CommandTasks)}
	}
	if verb == taskList {
		return c.tasksList(req, title)
	}
	tracked, found := c.taskTarget(req.ConversationID, id, verb)
	if !found {
		switch {
		case id != "":
			return Result{Title: title, Text: c.text.T(i18n.TaskUnknown, id)}
		case verb == taskResume:
			return Result{Title: title, Text: c.text.T(i18n.TaskNonePaused, protocol.CommandTasks)}
		default:
			return Result{Title: title, Text: c.text.T(i18n.TaskNone)}
		}
	}
	switch verb {
	case taskShow:
		return Result{Title: title, Text: c.taskDetail(tracked)}
	case taskPause:
		return c.taskSetAside(ctx, title, tracked, task.StatePaused)
	case taskCancel:
		return c.taskSetAside(ctx, title, tracked, task.StateCancelled)
	case taskResume:
		return c.taskPickUp(title, tracked)
	}
	return Result{Title: title, Text: c.text.T(i18n.TasksUsage, protocol.CommandTasks)}
}

// taskTarget resolves which task the user meant. An explicit id is scoped to
// this conversation on purpose: task ids are short and guessable, and one chat
// must not be able to reach into another's work.
func (c commands) taskTarget(conversationID, id string, verb taskVerb) (task.Task, bool) {
	if id != "" {
		tracked, ok := c.tasks.Get(id)
		if !ok || tracked.Channel != conversationID {
			return task.Task{}, false
		}
		return tracked, true
	}
	// List is newest first, so the bare verb acts on what the user most
	// plausibly has in mind — the thing they were just talking about.
	for _, candidate := range c.tasks.List(conversationID) {
		if verb == taskResume {
			if candidate.State == task.StatePaused {
				return candidate, true
			}
			continue
		}
		if !candidate.State.Terminal() {
			return candidate, true
		}
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

// taskPickUp puts a set-aside task back in play. The state moves before the
// re-entry so the replayed message finds it as this conversation's active task
// instead of opening a second task for the same work.
func (c commands) taskPickUp(title string, tracked task.Task) Result {
	moved, err := c.tasks.Advance(tracked.ID, task.StateRunning)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.TaskStuck, tracked.ID, statusMark(tracked.State))}
	}
	if c.resumer == nil || moved.AnchorMessage == "" || moved.Member == "" {
		// Nothing can replay a message here; the task is live again, so
		// the user's next message continues it.
		return Result{Title: title, Text: c.text.T(i18n.TaskResumed, moved.ID)}
	}
	c.resumer(TaskResume{
		TaskID: moved.ID, Goal: moved.Goal, Member: moved.Member,
		ConversationID: moved.Channel, ChatID: moved.ChatID,
		MessageID: moved.AnchorMessage, Requester: moved.Requester,
		ChatType: moved.ChatType,
	})
	// The re-entry announces itself at the anchor, so this card carries the
	// detail instead of repeating the announcement.
	return Result{Title: title, Text: c.taskDetail(moved)}
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

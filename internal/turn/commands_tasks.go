// The /tasks command: listing, inspecting, pausing, stopping and picking
// up tracked tasks by hand.

package turn

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"strings"
	"time"
)

type taskVerb = protocol.TaskVerb

func parseTaskArgs(rest string) (taskVerb, string, bool) { return protocol.ParseTaskArgs(rest) }
func isTaskID(id string) bool                            { return protocol.IsTaskID(id) }

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
		return c.tasksList(ctx, req, title)
	}
	tracked, found := c.taskTarget(req, id, verb)
	if found {
		want := project.RoleWrite
		if verb == taskShow {
			want = project.RoleRead
		}
		if _, checkErr := c.controls.Task(ctx, req, tracked.ID, want); checkErr != nil {
			return Result{}, checkErr
		}
	}
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
		return c.taskSetAside(ctx, title, tracked, task.StatePaused)
	case taskCancel:
		return c.taskSetAside(ctx, title, tracked, task.StateCancelled)
	case taskResume:
		return c.taskPickUp(ctx, req, title, tracked)
	case taskComplete:
		return c.taskComplete(ctx, req, title, tracked)
	case taskHandled:
		return c.taskSettle(ctx, req, title, tracked, task.SettlementHandled)
	case taskIgnored:
		return c.taskSettle(ctx, req, title, tracked, task.SettlementIgnored)
	case taskReopen:
		return c.taskSettle(ctx, req, title, tracked, "")
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
		if !ok || tracked.Transport != req.Source.Channel || (tracked.Channel == "" || tracked.Channel != req.Source.ConversationID) {
			return task.Task{}, false
		}
		return tracked, true
	}
	// List is newest first, so the bare verb acts on what the user most
	// plausibly has in mind — the thing they were just talking about.
	var completed task.Task
	for _, candidate := range c.tasks.List(req.Source.ConversationID) {
		if candidate.Transport != req.Source.Channel {
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
func (c commands) taskSetAside(ctx context.Context, title string, tracked task.Task, to task.State) (Result, error) {
	result, _, err := c.setTaskAside(ctx, title, tracked, to, false)
	return result, err
}

// taskSettle records what the user decided about a failed task: they dealt
// with it, or it does not matter, or they want it back in front of them. The
// task keeps its state — the record has to keep saying the work failed — so
// this writes the decision beside it rather than moving it somewhere tidier.
func (c commands) taskSettle(ctx context.Context, req Request, title string, tracked task.Task, as task.Settlement) (Result, error) {
	settled, err := c.controls.Settle(ctx, req, tracked.ID, as)
	if err != nil {
		if errors.Is(err, task.ErrSettleState) {
			return Result{Title: title, Text: c.text.T(i18n.TaskSettleState, tracked.ID, statusMark(tracked.State))}, nil
		}
		return Result{}, err
	}
	switch as {
	case task.SettlementHandled:
		return Result{Title: title, Text: c.text.T(i18n.TaskHandled, settled.ID, protocol.CommandTasks)}, nil
	case task.SettlementIgnored:
		return Result{Title: title, Text: c.text.T(i18n.TaskIgnored, settled.ID, protocol.CommandTasks)}, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskReopened, settled.ID)}, nil
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
	identity := req.Source.MessageID
	if req.Admission.ExchangeID != "" {
		identity = req.Admission.ExchangeID
	}
	if identity == "" {
		return result, fmt.Errorf("durable resume requires a stable control input identity")
	}
	id := fmt.Sprintf("task-resume:%x", sha256.Sum256([]byte(req.Source.Channel+"\x00"+req.Source.ConversationID+"\x00"+identity)))
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
	if err := c.routes.resume(resume); err != nil {
		if errors.Is(err, ErrResumeAwaitsStop) {
			return result, UserError{Text: c.text.T(i18n.TaskResumeAwaitsStop, tracked.ID, c.text.T(i18n.ConsoleStopRecheckNow), protocol.CommandTasks)}
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	moved, err := c.controls.Resume(ctx, req, tracked, admission)
	if err != nil {
		return result, err
	}
	result.Text = c.taskDetail(moved)
	// Clear before waking a consumer. It may synchronously reserve this
	// same slot, but no channel callback is permission to execute.
	c.clearActive(tracked.Channel, tracked.Member)
	reserved = false
	c.routes.dispatch(resume)
	return result, nil
}

// tasksList is what this conversation has been working on. The listing is the
// whole point of a task outliving its turn, so it is a command rather than
// something only the debug API can see.
func (c commands) tasksList(ctx context.Context, req Request, title string) (Result, error) {
	all, err := c.controls.ListTasks(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if len(all) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.TasksEmpty)}, nil
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
	return Result{Title: title, Text: b.String()}, nil
}

// tasksListed keeps the card inside its byte budget; the rest are a count.
const tasksListed = 8

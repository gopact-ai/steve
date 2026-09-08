package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/text"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/project"
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
func (c *Coordinator) beginTask(req Request, selected agent.Agent, prompt string, binding project.Binding, workspace string) (string, error) {
	if c.tasks == nil {
		return "", nil
	}
	// An onboarding turn runs under a synthetic conversation that is
	// relocated afterwards; a task opened there would be orphaned as
	// forever-running, because task channels do not follow the relocation.
	if strings.HasPrefix(req.ConversationID, onboard.PendingPrefix) {
		return "", nil
	}
	executionNode := selected.Node
	if executionNode == "" {
		executionNode = c.node
	}
	tracked, ok := c.tasks.Active(req.ConversationID, selected.ID, req.Origin)
	if ok && tracked.ProjectID != "" && binding.ProjectID != "" && tracked.ProjectID != binding.ProjectID {
		// The binding moved under a task that was never closed (an older
		// switch, a crash between the two): the task stays with its
		// project, and this turn opens its own.
		slog.Warn(fmt.Sprintf("turn: task %s belongs to project %s, conversation now on %s; closing it", tracked.ID, tracked.ProjectID, binding.ProjectID), "task", tracked.ID, "conversation", req.ConversationID, "project", binding.ProjectID)
		if _, err := c.tasks.Advance(tracked.ID, task.StateDone); err != nil {
			slog.Error(fmt.Sprintf("turn: close task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", req.ConversationID)
		}
		ok = false
	}
	if !ok {
		created, err := c.tasks.Create(task.Task{
			Goal:      goal(prompt),
			Requester: req.SenderOpenID,
			Channel:   req.ConversationID,
			Member:    selected.ID,
			Node:      executionNode,
			Origin:    req.Origin,
			ProjectID: binding.ProjectID,
			Workspace: workspace,
		})
		if err != nil {
			// Losing the task record must not cost the user their turn.
			slog.Error(fmt.Sprintf("turn: create task: %v", err), "conversation", req.ConversationID, "agent", selected.ID, "node", executionNode)
			return "", nil
		}
		tracked = created
	}
	if _, err := c.tasks.Begin(tracked.ID, selected.ID, executionNode, ""); err != nil {
		if text, spent := c.budgetStop(tracked); spent {
			return "", UserError{Text: text}
		}
		slog.Error(fmt.Sprintf("turn: begin task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", req.ConversationID, "node", executionNode)
		return "", nil
	}
	// The anchor is what a restarted gateway replies to when it resumes
	// this task; refresh it every turn so delivery lands by the newest
	// exchange (and inside the right topic).
	if req.MessageID != "" {
		if err := c.tasks.SetAnchor(tracked.ID, req.ChatID, req.MessageID, string(req.ChatType), req.CardID); err != nil {
			slog.Error(fmt.Sprintf("turn: anchor task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", req.ConversationID)
		}
	}
	return tracked.ID, nil
}

// finishTask closes the attempt with what the turn cost and the model it
// ran on, as the progress stream reported them.
func (c *Coordinator) finishTask(id string, turnErr error, tokens task.Tokens, model string) {
	if c.tasks == nil || id == "" {
		return
	}
	if _, err := c.tasks.FinishAs(id, outcome(turnErr), tokens, 0, model); err != nil {
		slog.Error(fmt.Sprintf("turn: finish task %s: %v", id, err), "task", id)
	}
}

// closeTask ends the member's tasks on a session reset. /new means "start
// over", and a task that survived the session it was attempted through would
// silently keep charging turns against work nobody is doing any more. Every
// lineage goes, unattended ones included: they all ran through the session
// that has just been archived.
func (c *Coordinator) closeTask(conversationID, agentID string) {
	if c.tasks == nil {
		return
	}
	for _, tracked := range c.tasks.Holding(conversationID, agentID) {
		if _, err := c.tasks.Advance(tracked.ID, task.StateDone); err != nil {
			slog.Error(fmt.Sprintf("turn: close task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", conversationID, "agent", agentID)
		}
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
	line := strings.TrimSpace(text.FirstLine(strings.TrimSpace(prompt)))
	return text.Clip(line, goalLimit)
}

// budgetStop names the limit that stopped the task and shows where the work
// got to. A brake that only says "no" leaves the user guessing whether an hour
// of work survived; the digest is the difference between a stop and a loss.
// budgetTurns and budgetElapsed say where a task stands against its
// budget, or just where it stands when it has none.
func budgetTurns(b task.Budget) string {
	if b.MaxTurns <= 0 {
		return fmt.Sprintf("%d", b.Turns)
	}
	return fmt.Sprintf("%d/%d", b.Turns, b.MaxTurns)
}

func budgetElapsed(b task.Budget) string {
	if b.MaxElapsed <= 0 {
		return b.Elapsed.Round(time.Second).String()
	}
	return fmt.Sprintf("%s/%s", b.Elapsed.Round(time.Second), b.MaxElapsed.Round(time.Minute))
}

func (c *Coordinator) budgetStop(tracked task.Task) (string, bool) {
	limit, spent := tracked.Budget.Exhausted()
	if !spent {
		return "", false
	}
	stop := c.text.T(i18n.BudgetElapsed, tracked.ID, tracked.Budget.MaxElapsed.Round(time.Minute), protocol.CommandNew)
	if limit == "turns" {
		stop = c.text.T(i18n.BudgetTurns, tracked.ID, tracked.Budget.MaxTurns, protocol.CommandNew)
	}
	return stop + "\n\n**" + c.text.T(i18n.TaskWhereItGot) + "**\n" + c.taskDetail(tracked), true
}

// taskFields surfaces the live budget on /status. Showing spend beside the
// limit is what turns the brake from a surprise into something the user can
// see coming.
func (c *Coordinator) taskFields(conversationID, agentID string) []view.Field {
	if c.tasks == nil {
		return nil
	}
	tracked, ok := c.tasks.Active(conversationID, agentID, "")
	if !ok {
		return nil
	}
	return []view.Field{
		{Label: "Task", Value: "#" + tracked.ID, IsMetric: true},
		{Label: "Turns", Value: budgetTurns(tracked.Budget), IsMetric: true},
		{Label: "Elapsed", Value: budgetElapsed(tracked.Budget), IsMetric: true},
		{Label: "Goal", Value: tracked.Goal, Wide: true},
	}
}

// TaskResume is the channel-side re-entry for a task the user picked back up.
// The coordinator cannot post messages itself, so it hands these fields to
// whoever owns the chat: post a notice at the task's anchor and replay it as
// a real message, and the resumed turn renders a card like any other turn.
type TaskResume struct {
	TaskID         string
	Goal           string
	Member         string
	ConversationID string
	ChatID         string
	MessageID      string
	Requester      string
	ChatType       string
}

// SetResumer wires that re-entry. Without it /tasks resume still un-pauses the
// task; the user's next message is what continues it.
func (c *Coordinator) SetResumer(fn func(TaskResume)) { c.resumer = fn }

// TaskNotice is a line Steve pushes into the chat on its own, outside any
// turn's card. It exists because delivery is a platform promise here: a task
// that ran for an hour and then ended must say so, whether or not the person
// who asked is still watching.
type TaskNotice struct {
	TaskID    string
	ChatID    string
	MessageID string
	Requester string
	// Conversation is the thread the task lives in: on the console, the
	// notice belongs there, not to whichever thread is open.
	Conversation string
	Text         string
}

// SetAfterTurn wires what runs once a turn's attempt is closed and its
// queued landings are done: the delegation service delivering the
// results of children that ended while the turn ran.
func (c *Coordinator) SetAfterTurn(fn func(taskID string)) { c.afterTurn = fn }

// SetNotifier wires those pushes to the channel. Nil simply means Steve keeps
// its news to the cards.
func (c *Coordinator) SetNotifier(fn func(TaskNotice)) { c.notifier = fn }

// SetOfflineReminder sets how long a turn must run before its completion also
// earns a plain-text ping. A non-positive value turns the ping off.
func (c *Coordinator) SetOfflineReminder(after time.Duration) { c.offlineAfter = after }

// noteActivity records that this conversation just heard from a person. The
// question the reminder has to answer is "did they walk away?", and the only
// evidence Steve has is whether anything arrived while the turn was running.
func (c *Coordinator) noteActivity(conversationID string) time.Time {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastSeen == nil {
		c.lastSeen = map[string]time.Time{}
	}
	c.lastSeen[conversationID] = now
	return now
}

func (c *Coordinator) heardSince(conversationID string, mark time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen[conversationID].After(mark)
}

// offlineReminder pings the asker when a long turn lands. The final card
// already carries a real @, but a card that arrives forty minutes later drops
// into a chat nobody is looking at; the plain-text line is the second, louder
// knock — and it is only sent when the person truly went quiet.
func (c *Coordinator) offlineReminder(req Request, id string, started time.Time, turnErr error) {
	if c.notifier == nil || c.tasks == nil || id == "" || c.offlineAfter <= 0 {
		return
	}
	if turnErr != nil {
		// A failed turn's card says so and @s the asker; a second line
		// repeating bad news is noise, not delivery.
		return
	}
	elapsed := time.Since(started)
	if elapsed < c.offlineAfter || c.heardSince(req.ConversationID, started) {
		return
	}
	c.notifier(TaskNotice{
		TaskID: id, ChatID: req.ChatID, MessageID: req.MessageID, Requester: req.SenderOpenID, Conversation: req.ConversationID,
		Text: c.text.T(i18n.TaskOfflineDone, id, elapsed.Round(time.Minute)),
	})
}

const (
	taskList taskVerb = iota
	taskShow
	taskPause
	taskResume
	taskCancel
)

func (c *Coordinator) setTaskAside(ctx context.Context, title string, tracked task.Task, to task.State, confirmSettlement bool) (Result, error) {
	var stopErr error
	if c.executions != nil {
		ids, err := c.tasks.SetAside(tracked.ID, to)
		if err != nil {
			return Result{Title: title, Text: err.Error()}, err
		}
		if c.attempts != nil {
			for _, id := range ids {
				if records, err := c.attempts.ForTask(ctx, id); err == nil {
					for _, record := range records {
						if !record.Unsettled && record.StopEvidence != "" {
							c.executions.Resolve(record.ID)
						}
					}
				}
			}
		}
		waitCtx, finishWait := context.WithTimeout(ctx, 20*time.Second)
		stopErr = c.executions.Stop(ids, task.ErrExecutionStopped).Wait(waitCtx)
		finishWait()
		if c.attempts != nil {
			for _, id := range ids {
				records, err := c.attempts.ForTask(context.WithoutCancel(ctx), id)
				if err != nil {
					stopErr = errors.Join(stopErr, err)
					continue
				}
				for _, record := range records {
					if record.Unsettled || (confirmSettlement && !record.State.Terminal()) {
						stopErr = errors.Join(stopErr, fmt.Errorf("attempt %s writer is quarantined until physically confirmed stopped", record.ID))
					}
				}
			}
		}
	} else {
		if _, err := c.tasks.Advance(tracked.ID, to); err != nil {
			return Result{Title: title, Text: c.text.T(i18n.TaskStuck, tracked.ID, statusMark(tracked.State))}, err
		}
		c.stopTurnFor(ctx, tracked)
	}
	moved, _ := c.tasks.Get(tracked.ID)
	if stopErr != nil {
		return Result{Title: title, Text: fmt.Sprintf("task #%s: stop recorded, execution has not confirmed stopping: %v", tracked.ID, stopErr)}, stopErr
	}
	// Re-read: the stopped turn closes its own attempt, and the detail is
	// only worth showing if it reflects that.
	if latest, ok := c.tasks.Get(moved.ID); ok {
		moved = latest
	}
	if to == task.StatePaused {
		return Result{Title: title, Text: c.text.T(i18n.TaskPaused, moved.ID, protocol.CommandTasks) + "\n\n" + c.taskDetail(moved)}, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskCancelled, moved.ID) + "\n\n" + c.taskDetail(moved)}, nil
}

// stopTurnFor stops the turn a task is running through, if any. The task's own
// member identifies that turn — by the time the user sets work aside they may
// well be talking to a different agent.
func (c *Coordinator) stopTurnFor(ctx context.Context, tracked task.Task) {
	member, ok := c.catalog.Resolve(tracked.Member)
	if !ok {
		return
	}
	c.mu.Lock()
	running := c.cancels[sessionKey(tracked.Channel, member.ID)] != nil
	c.mu.Unlock()
	if !running {
		return
	}
	if _, err := c.cancel(ctx, tracked.Channel, member); err != nil {
		slog.Error(fmt.Sprintf("turn: stop turn for task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", tracked.Channel, "agent", member.ID)
	}
}

func (c *Coordinator) advanceExecution(ctx context.Context, id string, to task.State) (task.Task, error) {
	if token := execution.Token(ctx); token != nil {
		return c.tasks.AdvanceExecution(*token, to)
	}
	return c.tasks.Advance(id, to)
}

// taskDetail is the progress view. The attempt trail is the honest part: it
// shows the interruptions and cancellations, not only the turns that worked.
func (c *Coordinator) taskDetail(tracked task.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**#%s** %s · %s · %s turns · %s",
		tracked.ID, statusMark(tracked.State), tracked.Member,
		budgetTurns(tracked.Budget), budgetElapsed(tracked.Budget))
	if tracked.Goal != "" {
		fmt.Fprintf(&b, "\n%s", tracked.Goal)
	}
	if trail := attemptTrail(tracked.Attempts); trail != "" {
		fmt.Fprintf(&b, "\n**%s**  %s", c.text.T(i18n.TaskAttempts), trail)
	}
	return b.String()
}

// attemptsShown bounds the trail; older attempts collapse into a count so a
// long task's detail stays inside the card's byte budget.
const attemptsShown = 10

func attemptTrail(attempts []task.Attempt) string {
	if len(attempts) == 0 {
		return ""
	}
	marks := make([]string, 0, attemptsShown+1)
	if extra := len(attempts) - attemptsShown; extra > 0 {
		marks = append(marks, fmt.Sprintf("…+%d", extra))
		attempts = attempts[extra:]
	}
	for _, attempt := range attempts {
		if attempt.Open() {
			marks = append(marks, "running")
			continue
		}
		marks = append(marks, string(attempt.Outcome))
	}
	return strings.Join(marks, " · ")
}

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
	case task.StatePaused:
		return "paused"
	case task.StateCancelled:
		return "cancelled"
	default:
		return string(state)
	}
}

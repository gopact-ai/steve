package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/text"
	"github.com/gopact-ai/steve/internal/view"
)

// goalLimit keeps a task listing readable. The full prompt is not lost: it
// belongs to the archive, which records every turn verbatim.
const goalLimit = 120

// beginTurnScope inherits authority only from work on the current project.
// A leftover task from another project is retired during admission; its
// revocation must stop the old work without cancelling this new turn. Capture
// the candidate before callbacks or I/O so a concurrent pause/resume cannot
// replace its original authorization with an empty or newer one.
func (c *Coordinator) beginTurnScope(ctx context.Context, req Request, agentID string) (project.Binding, *execution.Scope, error) {
	var previous task.Task
	previous, _ = c.tasks.Active(req.ConversationID, agentID, req.Origin)
	req.stage(view.StageWorkspace)
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		return binding, nil, err
	}
	taskID := ""
	if previous.ProjectID == "" || previous.ProjectID == binding.ProjectID {
		taskID = previous.ID
	}
	scope, err := c.executions.Begin(ctx, execution.Key{TaskID: taskID, InstanceID: req.MessageID})
	if err != nil {
		return binding, nil, err
	}
	if taskID != "" && scope.Token().Epoch != previous.ExecutionEpoch {
		scope.Finish(nil)
		return binding, nil, fmt.Errorf("%w: task %s changed during workspace preparation", task.ErrExecutionStopped, taskID)
	}
	return binding, scope, nil
}

// beginTask opens or continues the member's task on this channel and charges a
// turn to it. It returns an empty id when task tracking is disabled, and a
// UserError when the budget is spent. When tracking is configured, admission
// must be durable before any native execution can start.
func (c *Coordinator) beginTask(req Request, selected agent.Agent, prompt string, binding project.Binding, workspace string) (string, error) {
	if req.ResumeAdmission != (task.ResumeAdmission{}) && req.ExpectedTask != req.ResumeAdmission.TaskID {
		return "", fmt.Errorf("%w: resume input requires its original task", task.ErrExecutionStopped)
	}
	executionNode := selected.Node
	if executionNode == "" {
		executionNode = c.node
	}
	tracked, ok := c.tasks.Active(req.ConversationID, selected.ID, req.Origin)
	if req.ExpectedTask != "" {
		tracked, ok = c.tasks.Get(req.ExpectedTask)
		if !ok || tracked.Channel != req.ConversationID || tracked.Member != selected.ID || (tracked.ProjectID != "" && tracked.ProjectID != binding.ProjectID) {
			return "", fmt.Errorf("task %s continuation binding changed", req.ExpectedTask)
		}
		if tracked.State != task.StateRunning {
			return "", fmt.Errorf("%w: task %s is no longer available", task.ErrContinuationUnavailable, req.ExpectedTask)
		}
	}
	if ok && tracked.ProjectID != "" && binding.ProjectID != "" && tracked.ProjectID != binding.ProjectID {
		if err := c.releaseLeftover(req, tracked, binding.ProjectID); err != nil {
			return "", err
		}
		ok = false
	}
	if !ok {
		// The introduction is the platform's own turn, not work the owner
		// asked for: its task is accounted but never listed as theirs.
		title := goal(prompt)
		if onboarding(req) {
			title = onboard.TaskGoal(c.text.Locale())
		}
		created, err := c.tasks.Create(task.Task{
			Goal:      title,
			Requester: req.SenderOpenID,
			Channel:   req.ConversationID,
			Transport: req.Channel,
			ChatID:    req.ChatID, AnchorMessage: req.MessageID, ChatType: string(req.ChatType), OpenCard: req.CardID,
			Member:    selected.ID,
			Node:      executionNode,
			Origin:    req.Origin,
			System:    onboarding(req),
			ProjectID: binding.ProjectID,
			Workspace: workspace,
		})
		if err != nil {
			return "", fmt.Errorf("create task for conversation %s: %w", req.ConversationID, err)
		}
		tracked = created
	}
	if tracked.Transport != req.Channel {
		return "", fmt.Errorf("task %s transport changed", tracked.ID)
	}
	_, beginErr := c.tasks.BeginTurn(tracked.ID, selected.ID, executionNode, task.TurnInput{
		Address: req.Address(), ChatID: req.ChatID, ChatType: string(req.ChatType), CardID: req.CardID, Continuation: req.ExpectedTask != "",
		ResumeAdmission: req.ResumeAdmission, TurnID: req.MessageID,
	})
	if err := beginErr; err != nil {
		if req.ExpectedTask != "" {
			return "", err
		}
		if text, spent := c.budgetStop(tracked); spent {
			return "", UserError{Text: text}
		}
		return "", fmt.Errorf("admit turn for task %s: %w", tracked.ID, err)
	}
	return tracked.ID, nil
}

// onboarding reports a turn running under the synthetic conversation the
// first introduction uses before it is relocated into the real chat.
func onboarding(req Request) bool {
	return strings.HasPrefix(req.ConversationID, onboard.PendingPrefix)
}

// closeOnboardingTask ends the onboarding turn's task once the turn is
// accounted. The turn needs a task of its own, because a node authorizes a
// native session only for an attempt carrying a task execution token, but
// the task must not outlive the turn: its channel is the synthetic
// conversation, which the relocation into the real chat does not carry
// along. A failed turn leaves the task running, so the next onboarding
// attempt continues it instead of opening another, and the idle sweep
// closes it if onboarding never runs again.
func (c *Coordinator) closeOnboardingTask(req Request, id string, turnErr error) {
	if id == "" || turnErr != nil || !onboarding(req) {
		return
	}
	if _, err := c.tasks.Advance(id, task.StateDone); err != nil {
		slog.Error(fmt.Sprintf("turn: close onboarding task %s: %v", id, err), "task", id, "conversation", req.ConversationID)
	}
}

// releaseLeftover lets go of a task the binding moved under without it
// being closed (an older switch, a crash between the two): the task stays
// with its project, and this turn opens its own. One whose continuation is
// still unsettled keeps running until it settles, rather than ending as
// done under it.
func (c *Coordinator) releaseLeftover(req Request, tracked task.Task, projectID string) error {
	slog.Warn(fmt.Sprintf("turn: task %s belongs to project %s, conversation now on %s; closing it", tracked.ID, tracked.ProjectID, projectID), "task", tracked.ID, "conversation", req.ConversationID, "project", projectID)
	if setsAside(tracked) {
		if err := c.setAside(tracked); err != nil {
			return fmt.Errorf("close previous project task %s: %w", tracked.ID, err)
		}
		return nil
	}
	_, err := c.closeSettled(context.Background(), []string{tracked.ID}, req.ExchangeID)
	if task.CompletionRefused(err) {
		slog.Info(fmt.Sprintf("turn: task %s of project %s stays open: %v", tracked.ID, tracked.ProjectID, err), "task", tracked.ID, "conversation", req.ConversationID, "project", tracked.ProjectID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close previous project task %s: %w", tracked.ID, err)
	}
	return nil
}

// closeTask releases the tasks the agents hold in the conversation when
// their sessions are archived. Ordinary work ends as done, but only once
// nothing of it is left unsettled, by the ledger checks /complete makes: a
// task whose execution still runs, or still waits on the owner, would
// otherwise be continued under a task that says it finished. One such task
// refuses the whole release before anything changes; its id and the
// refusal are returned. Failed or blocked work is set aside for a
// deliberate resume, not reported as completed. Every lineage leaves the
// conversation slot, including unattended work, so new inputs cannot
// charge old work.
func (c *Coordinator) closeTask(ctx context.Context, conversationID string, agents []string, currentExchange string) (string, error) {
	var closing []string
	var aside []task.Task
	for _, agentID := range agents {
		for _, tracked := range c.tasks.Holding(conversationID, agentID) {
			if setsAside(tracked) {
				aside = append(aside, tracked)
			} else {
				closing = append(closing, tracked.ID)
			}
		}
	}
	if refused, err := c.closeSettled(ctx, closing, currentExchange); err != nil {
		return refused, err
	}
	for _, tracked := range aside {
		if err := c.setAside(tracked); err != nil {
			slog.Error(fmt.Sprintf("turn: close task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", conversationID, "agent", tracked.Member)
		}
	}
	return "", nil
}

// setsAside reports whether releasing the task sets it aside. Resetting or
// switching projects cannot turn blocked or failed work into a success.
func setsAside(tracked task.Task) bool {
	return tracked.State == task.StateBlocked || tracked.State == task.StateFailed
}

// setAside keeps blocked or failed work available to resume, with its
// original history, but revokes its old execution and releases the
// conversation slot.
func (c *Coordinator) setAside(tracked task.Task) error {
	ids, err := c.tasks.SetAside(tracked.ID, task.StatePaused)
	if err != nil {
		return err
	}
	c.executions.Stop(ids, task.ErrExecutionStopped)
	return nil
}

// closeSettled ends the tasks as done, all together, once the completion
// check finds nothing of any of them unsettled. Each is checked alone, not
// with the work it delegated: a child keeps running and delivering on its
// own terms. currentExchange is the input asking for the close, which the
// check spares; so are the conversation's lines still queued that no task
// has claimed, such as input typed behind that one: they have not started,
// and run after the close under whichever task then holds the
// conversation. On a refusal the task it is about is returned with it: the
// one the check refused, or the only one closing. A close of several that
// fails otherwise is about none of them, and no task is returned.
func (c *Coordinator) closeSettled(ctx context.Context, ids []string, currentExchange string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	var refused string
	_, err := c.tasks.CloseChecked(ctx, ids, func(tx *ledger.Tx, tracked task.Task) error {
		err := c.checkTaskCompletionTx(tx, map[string]bool{tracked.ID: true}, tracked.Channel, currentExchange, true)
		if err != nil {
			refused = tracked.ID
		}
		return err
	})
	if err == nil {
		return "", nil
	}
	if refused == "" && len(ids) == 1 {
		refused = ids[0]
	}
	return refused, err
}

// closeRefusal tells the user why a reset or a project switch did nothing,
// and how to let the task go: settle what it still waits for, or cancel it.
// Without taskID the close of the conversation's tasks failed as a whole.
func (c *Coordinator) closeRefusal(conversationID, taskID string, err error) UserError {
	if taskID == "" {
		slog.Warn(fmt.Sprintf("turn: close tasks: %v", err), "conversation", conversationID)
		return UserError{Text: c.text.T(i18n.TaskCloseSeveralFailed, protocol.CommandTasks)}
	}
	key := i18n.TaskCloseFailed
	switch {
	case errors.Is(err, task.ErrCompleteBusy):
		key = i18n.TaskCloseBusy
	case errors.Is(err, task.ErrCompleteDelivery):
		key = i18n.TaskCloseDelivery
	case errors.Is(err, task.ErrCompleteAttention):
		key = i18n.TaskCloseAttention
	default:
		slog.Warn(fmt.Sprintf("turn: close task %s: %v", taskID, err), "task", taskID, "conversation", conversationID)
	}
	return UserError{Text: c.text.T(key, taskID, protocol.CommandTasks)}
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
	Admission      task.ResumeAdmission
	Transport      string
	TaskID         string
	Goal           string
	Member         string
	ConversationID string
	ChatID         string
	MessageID      string
	Requester      string
	ChatType       string
}

// TaskNotice is a line Steve pushes into the chat on its own, outside any
// turn's card. It exists because delivery is a platform promise here: a task
// that ran for an hour and then ended must say so, whether or not the person
// who asked is still watching.
type TaskNotice struct {
	Transport string
	TaskID    string
	ChatID    string
	MessageID string
	Requester string
	// Conversation is the thread the task lives in: on the console, the
	// notice belongs there, not to whichever thread is open.
	Conversation string
	Text         string
}

// TaskCancel names the tasks a person just cancelled: the one they chose
// and every task delegated under it. None of them can be resumed any more,
// so whatever the channel still holds open to resume one — a recovery
// question offering to retry it — is waiting for an answer that cannot
// help.
type TaskCancel struct {
	Transport string
	Tasks     []string
}

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
	if id == "" || c.offlineAfter <= 0 {
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
	c.routes.notify(TaskNotice{
		TaskID: id, Transport: req.Channel, ChatID: req.ChatID, MessageID: req.MessageID, Requester: req.SenderOpenID, Conversation: req.ConversationID,
		Text: c.text.T(i18n.TaskOfflineDone, id, elapsed.Round(time.Minute)),
	})
}

const (
	taskList taskVerb = iota
	taskShow
	taskPause
	taskResume
	taskCancel
	taskComplete
	// taskHandled and taskIgnored are the two ways a person closes a
	// failed task without pretending it was called off; taskReopen puts
	// it back in front of them.
	taskHandled
	taskIgnored
	taskReopen
)

// setTaskAside moves tracked and the tasks under it to to and stops what
// runs under them. It returns the tasks it moved, which a failed stop does
// not take back.
func (c *Coordinator) setTaskAside(ctx context.Context, title string, tracked task.Task, to task.State, confirmSettlement bool) (Result, []string, error) {
	ids, err := c.tasks.SetAside(tracked.ID, to)
	if err != nil {
		return Result{Title: title, Text: err.Error()}, nil, err
	}
	stopErr := c.stopExecutions(ctx, ids, confirmSettlement)
	moved, _ := c.tasks.Get(tracked.ID)
	if stopErr != nil {
		return Result{Title: title, Text: fmt.Sprintf("task #%s: stop recorded, execution has not confirmed stopping: %v", tracked.ID, stopErr)}, ids, stopErr
	}
	// Re-read: the stopped turn closes its own attempt, and the detail is
	// only worth showing if it reflects that.
	if latest, ok := c.tasks.Get(moved.ID); ok {
		moved = latest
	}
	if to == task.StatePaused {
		return Result{Title: title, Text: c.text.T(i18n.TaskPaused, moved.ID, protocol.CommandTasks) + "\n\n" + c.taskDetail(moved)}, ids, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskCancelled, moved.ID) + "\n\n" + c.taskDetail(moved)}, ids, nil
}

// stopExecutions stops what is running under tasks whose execution has
// just been revoked by SetAside, and waits for the stop to be confirmed.
// An error says the stop is recorded but some writer has not been seen to
// stop; with confirmSettlement, an attempt still open counts as that.
func (c *Coordinator) stopExecutions(ctx context.Context, ids []string, confirmSettlement bool) error {
	var stopErr error
	for _, id := range ids {
		if records, err := c.attempts.ForTask(ctx, id); err == nil {
			for _, record := range records {
				if !record.Unsettled && record.StopEvidence != "" {
					stopErr = errors.Join(stopErr, c.resolveStoppedExecution(ctx, record))
				}
			}
		} else {
			stopErr = errors.Join(stopErr, err)
		}
	}
	waitCtx, finishWait := context.WithTimeout(ctx, 20*time.Second)
	stopErr = errors.Join(stopErr, c.executions.Stop(ids, task.ErrExecutionStopped).Wait(waitCtx))
	finishWait()
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
	return stopErr
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

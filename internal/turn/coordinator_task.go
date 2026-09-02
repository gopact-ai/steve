package turn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
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
	tracked, ok := c.tasks.Active(req.ConversationID, selected.ID, req.Origin)
	if !ok {
		created, err := c.tasks.Create(task.Task{
			Goal:      goal(prompt),
			Requester: req.SenderOpenID,
			Channel:   req.ConversationID,
			Member:    selected.ID,
			Node:      c.node,
			Origin:    req.Origin,
			ProjectID: binding.ProjectID,
			Workspace: workspace,
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
	// The anchor is what a restarted gateway replies to when it resumes
	// this task; refresh it every turn so delivery lands by the newest
	// exchange (and inside the right topic).
	if req.MessageID != "" {
		if err := c.tasks.SetAnchor(tracked.ID, req.ChatID, req.MessageID, string(req.ChatType), req.CardID); err != nil {
			log.Printf("turn: anchor task %s: %v", tracked.ID, err)
		}
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
			log.Printf("turn: close task %s: %v", tracked.ID, err)
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
	trimmed := strings.TrimSpace(prompt)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
	if len([]rune(trimmed)) <= goalLimit {
		return trimmed
	}
	return string([]rune(trimmed)[:goalLimit]) + "…"
}

// budgetStop names the limit that stopped the task and shows where the work
// got to. A brake that only says "no" leaves the user guessing whether an hour
// of work survived; the digest is the difference between a stop and a loss.
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
		{Label: "Turns", Value: fmt.Sprintf("%d/%d", tracked.Budget.Turns, tracked.Budget.MaxTurns), IsMetric: true},
		{Label: "Elapsed", Value: fmt.Sprintf("%s/%s",
			tracked.Budget.Elapsed.Round(time.Second), tracked.Budget.MaxElapsed.Round(time.Minute)), IsMetric: true},
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
	Text      string
}

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
		TaskID: id, ChatID: req.ChatID, MessageID: req.MessageID, Requester: req.SenderOpenID,
		Text: c.text.T(i18n.TaskOfflineDone, id, elapsed.Round(time.Minute)),
	})
}

// taskVerb is what the user asked to do to a task.
type taskVerb int

const (
	taskList taskVerb = iota
	taskShow
	taskPause
	taskResume
	taskCancel
)

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
func (c *Coordinator) tasksCmd(ctx context.Context, req Request, rest string) Result {
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
func (c *Coordinator) taskTarget(conversationID, id string, verb taskVerb) (task.Task, bool) {
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
func (c *Coordinator) taskSetAside(ctx context.Context, title string, tracked task.Task, to task.State) Result {
	moved, err := c.tasks.Advance(tracked.ID, to)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.TaskStuck, tracked.ID, statusMark(tracked.State))}
	}
	c.stopTurnFor(ctx, moved)
	// Re-read: the stopped turn closes its own attempt, and the detail is
	// only worth showing if it reflects that.
	if latest, ok := c.tasks.Get(moved.ID); ok {
		moved = latest
	}
	if to == task.StatePaused {
		return Result{Title: title, Text: c.text.T(i18n.TaskPaused, moved.ID, protocol.CommandTasks) + "\n\n" + c.taskDetail(moved)}
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskCancelled, moved.ID) + "\n\n" + c.taskDetail(moved)}
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
		log.Printf("turn: stop turn for task %s: %v", tracked.ID, err)
	}
}

// taskPickUp puts a set-aside task back in play. The state moves before the
// re-entry so the replayed message finds it as this conversation's active task
// instead of opening a second task for the same work.
func (c *Coordinator) taskPickUp(title string, tracked task.Task) Result {
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

// taskDetail is the progress view. The attempt trail is the honest part: it
// shows the interruptions and cancellations, not only the turns that worked.
func (c *Coordinator) taskDetail(tracked task.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**#%s** %s · %s · %d/%d turns · %s/%s",
		tracked.ID, statusMark(tracked.State), tracked.Member,
		tracked.Budget.Turns, tracked.Budget.MaxTurns,
		tracked.Budget.Elapsed.Round(time.Second), tracked.Budget.MaxElapsed.Round(time.Minute))
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

// tasksList is what this conversation has been working on. The listing is the
// whole point of a task outliving its turn, so it is a command rather than
// something only the debug API can see.
func (c *Coordinator) tasksList(req Request, title string) Result {
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
		fmt.Fprintf(&b, "**#%s** %s · %s · %d/%d turns · %s",
			tracked.ID, statusMark(tracked.State), tracked.Member,
			tracked.Budget.Turns, tracked.Budget.MaxTurns,
			tracked.Budget.Elapsed.Round(time.Second))
		if tracked.Goal != "" {
			fmt.Fprintf(&b, "\n%s", tracked.Goal)
		}
	}
	return Result{Title: title, Text: b.String()}
}

// tasksListed keeps the card inside its byte budget; the rest are a count.
const tasksListed = 8

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

// The /every, /at and /schedules commands.

package turn

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/schedule"
	"strings"
	"time"
)

// scheduleCmd creates standing work. "/every 30m look at CI" and "/at 09:00
// summarise yesterday" are the whole surface: the spec is whatever leads, the
// instruction is the rest, and the instruction is stored verbatim so it fires
// as the message the user would have typed themselves.
func (c commands) scheduleCmd(req Request, selected agent.Agent, cmd protocol.Command, rest string) Result {
	title := c.text.T(i18n.CardSchedules)
	if req.Origin != "" {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleNotCreatedAutomatic)}
	}
	if strings.TrimSpace(rest) == "" {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleUsage, cmd)}
	}
	now := time.Now()
	parse := schedule.ParseEvery
	if cmd == protocol.CommandAt {
		parse = schedule.ParseAt
	}
	spec, prompt, err := parse(rest, now)
	if err != nil {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleBad, firstWord(rest))}
	}
	// An anchor is not optional: without a message to reply to, the firing
	// would have nowhere to land and the schedule would run in silence.
	if req.MessageID == "" {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleUsage, cmd)}
	}
	if req.Channel == "" {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleNotCreatedNoChannel)}
	}
	binding, err := c.bindingFor(context.Background(), req)
	if err != nil {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleNotCreated, err.Error())}
	}
	if err := checkScheduledProject(req.ExpectedProject, binding.ProjectID); err != nil {
		return Result{AgentID: selected.ID, Title: title, Text: c.text.T(i18n.ScheduleNotCreated, err.Error())}
	}
	created, err := c.schedules.Create(schedule.Job{
		Channel: req.Channel, ProjectID: binding.ProjectID,
		ConversationID: req.ConversationID,
		ChatID:         req.ChatID,
		ChatType:       string(req.ChatType),
		AnchorMessage:  req.MessageID,
		Requester:      req.SenderOpenID,
		Member:         selected.ID,
		Prompt:         prompt,
		Spec:           spec,
	})
	if err != nil {
		return Result{AgentID: selected.ID, Title: title,
			Text: c.text.T(i18n.ScheduleRefused, schedule.MaxPerConversation, protocol.CommandSchedules)}
	}
	return Result{AgentID: selected.ID, Title: title, Text: fmt.Sprintf("%s\n%s",
		c.text.T(i18n.ScheduleCreated, created.ID, c.specLabel(created.Spec), c.when(created.NextAt)),
		created.Prompt)}
}

// schedulesCmd lists and cancels. It reuses the task verbs on purpose: one
// vocabulary for "show me" and "call it off", whichever kind of standing work
// the user has in mind.
func (c commands) schedulesCmd(req Request, rest string) Result {
	title := c.text.T(i18n.CardSchedules)
	fields := strings.Fields(rest)
	if verdict, ok := firingVerdict(fields); ok {
		id := strings.TrimPrefix(fields[1], "#")
		job, ok := c.schedules.Get(id)
		if !ok || job.ConversationID != req.ConversationID {
			return Result{Title: title, Text: c.text.T(i18n.ScheduleUnknown, id)}
		}
		if req.SenderOpenID == "" || (req.SenderOpenID != job.Requester && req.SenderOpenID != c.ownerOpenID) {
			return Result{Title: title, Text: c.text.T(i18n.ScheduleResolveForbidden)}
		}
		if err := c.schedules.ResolveFiring(id, verdict, req.SenderOpenID); err != nil {
			return Result{Title: title, Text: err.Error()}
		}
		if verdict == "retry" {
			return Result{Title: title, Text: c.text.T(i18n.ScheduleRetryGranted, id)}
		}
		return Result{Title: title, Text: c.text.T(i18n.ScheduleRunConfirmed, id)}
	}
	verb, id, ok := parseTaskArgs(rest)
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.ScheduleUsage, protocol.CommandEvery)}
	}
	if verb == taskCancel {
		return c.cancelSchedule(req.ConversationID, id, title)
	}
	jobs := c.schedules.List(req.ConversationID)
	if len(jobs) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.SchedulesEmpty, protocol.CommandEvery)}
	}
	var b strings.Builder
	for i, job := range jobs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**#%s** %s · %s %s", job.ID, c.specLabel(job.Spec),
			c.text.T(i18n.ScheduleNextLabel), c.when(job.NextAt))
		if job.Runs > 0 {
			fmt.Fprintf(&b, " · %s", c.text.T(i18n.ScheduleRunsLabel, job.Runs))
		}
		fmt.Fprintf(&b, " · %s\n%s", job.Member, job.Prompt)
		if job.PendingKey != "" {
			b.WriteString("\n" + c.text.T(i18n.ScheduleStateLabel, job.State))
			if job.Error != "" {
				fmt.Fprintf(&b, " · %s", job.Error)
			}
			if job.State == schedule.FiringUnknown {
				b.WriteString("\n" + c.text.T(i18n.ScheduleCheckFiring, job.ID, job.ID))
			}
		}
	}
	return Result{Title: title, Text: b.String()}
}

// firingVerdict reads "confirm <id>" or "retry <id>", in either language.
func firingVerdict(fields []string) (string, bool) {
	if len(fields) != 2 {
		return "", false
	}
	switch fields[0] {
	case "confirm", "确认":
		return "confirm", true
	case "retry", "重试":
		return "retry", true
	}
	return "", false
}

func (c commands) cancelSchedule(conversationID, id, title string) Result {
	if id == "" {
		return Result{Title: title, Text: c.text.T(i18n.ScheduleUsage, protocol.CommandSchedules)}
	}
	// Scoped to this conversation for the same reason task ids are: the id
	// is short, and a schedule is someone else's standing instruction.
	if job, ok := c.schedules.Get(id); !ok || job.ConversationID != conversationID {
		return Result{Title: title, Text: c.text.T(i18n.ScheduleUnknown, id)}
	}
	removed, ok, err := c.schedules.Delete(id)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.ScheduleCancelFailed, id, err)}
	}
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.ScheduleUnknown, id)}
	}
	return Result{Title: title, Text: c.text.T(i18n.ScheduleCancelled, removed.ID) + "\n" + removed.Prompt}
}

func (c commands) specLabel(spec schedule.Spec) string {
	switch spec.Kind {
	case schedule.KindEvery:
		return c.text.T(i18n.ScheduleEveryLabel, spec.Every)
	case schedule.KindDaily:
		day := c.text.T(i18n.ScheduleDailyLabel, spec.Text, spec.Hour, spec.Minute)
		return day
	default:
		return c.text.T(i18n.ScheduleOnceLabel)
	}
}

// when keeps the clock human: today's firings read as a time, later ones carry
// the date they land on.
func (c commands) when(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	local := at.Local()
	if local.YearDay() == time.Now().YearDay() && local.Year() == time.Now().Year() {
		return local.Format("15:04")
	}
	return local.Format("01-02 15:04")
}

func firstWord(input string) string {
	if fields := strings.Fields(input); len(fields) > 0 {
		return fields[0]
	}
	return input
}

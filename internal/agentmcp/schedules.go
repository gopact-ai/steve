package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/schedule"
)

const scheduleInstructions = `## Scheduling (steve_schedule / steve_schedules / steve_schedule_cancel)
- When the user asks for a reminder, later follow-up, or recurring work, use steve_schedule; do not claim scheduling is unavailable or merely promise to remember.
- Pass mode "at" for a one-shot or "every" for recurring work, when for the timing only, and prompt for the work to execute. Examples: at + "30m", at + "2026-10-01 09:00", every + "1h", every + "day 09:00", every + "Mon 09:00". Wall-clock times use the server's local timezone; verify the returned next_at and timezone with the user when ambiguous.
- Use a stable idempotency_key for each user request. Reuse it with identical arguments for retries; never invent a new key to bypass an unknown outcome. Query steve_schedules first when a creation result is uncertain.
- These tools are for the owner's private conversation, not delegated children. The server fixes the conversation, channel, requester, project, agent and reply anchor; you cannot choose another destination.
- A scheduled firing must execute its stored work, not create more schedules. Scheduling from unattended work is refused.
- steve_schedules lists this conversation's schedules; steve_schedule_cancel(id) cancels one returned ID. Cancellation does not undo work already accepted. An uncertain firing requires the user's explicit /schedules confirm or retry; do not resolve it yourself.
- Report the returned schedule ID and next_at only after successful creation. A replay returns the original creation receipt, not proof that the schedule is still active; use steve_schedules for current state.`

// ScheduleRequest contains only user intent. Routing and authority are resolved
// by the coordinator from the authenticated binding and fixed execution scope.
type ScheduleRequest struct {
	Mode           string `json:"mode"`
	When           string `json:"when"`
	Prompt         string `json:"prompt"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Parse uses the same grammar as /at and /every, without allowing the timing
// field to smuggle text into the instruction.
func (r ScheduleRequest) Parse(now time.Time) (schedule.Spec, error) {
	if strings.TrimSpace(r.Prompt) == "" || strings.TrimSpace(r.When) == "" {
		return schedule.Spec{}, errors.New("when and prompt are required")
	}
	parse := schedule.ParseAt
	switch r.Mode {
	case "at":
	case "every":
		parse = schedule.ParseEvery
	default:
		return schedule.Spec{}, errors.New(`mode must be "at" or "every"`)
	}
	const marker = "__steve_schedule_instruction__"
	spec, rest, err := parse(strings.TrimSpace(r.When)+" "+marker, now)
	if err != nil {
		return schedule.Spec{}, err
	}
	if rest != marker {
		return schedule.Spec{}, errors.New("when must contain only a schedule time, not instructions")
	}
	if spec.Next(now).IsZero() {
		return schedule.Spec{}, errors.New("schedule would never fire")
	}
	return spec, nil
}

type Scheduler interface {
	// Authorization runs even for receipt replays: the current turn cannot
	// inherit the permissions of the task's original scheduling caller.
	AuthorizeSchedules(context.Context, Binding, bool) error
	Schedule(context.Context, Binding, ScheduleRequest) (schedule.Job, error)
	Schedules(context.Context, Binding) ([]schedule.Job, error)
	CancelSchedule(context.Context, Binding, string) (schedule.Job, error)
}

func (s *Server) SetScheduler(scheduler Scheduler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduler = scheduler
}

func decodeScheduleArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("schedule arguments must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("bad schedule arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("schedule arguments must be one object")
	}
	return nil
}

func (s *Server) scheduleCall(ctx context.Context, bind binding, tool string, raw json.RawMessage) (string, error) {
	if bind.taskID != "" || bind.delegatedBy != "" {
		return "", errors.New("delegated tasks cannot manage schedules; ask the parent")
	}
	if _, ok := ScopeFromContext(ctx); !ok {
		return "", errors.New("scheduling requires an authorized fixed execution")
	}
	s.mu.Lock()
	scheduler := s.scheduler
	s.mu.Unlock()
	if scheduler == nil {
		return "", errors.New("scheduling is not wired on this gateway")
	}
	b := publicBinding(bind)
	if err := scheduler.AuthorizeSchedules(ctx, b, tool == "steve_schedule"); err != nil {
		return "", err
	}
	switch tool {
	case "steve_schedules":
		if err := decodeScheduleArgs(raw, &struct{}{}); err != nil {
			return "", err
		}
		jobs, err := scheduler.Schedules(ctx, b)
		if err != nil {
			return "", err
		}
		out := make([]scheduleView, 0, len(jobs))
		for _, job := range jobs {
			out = append(out, describeSchedule(job))
		}
		return jsonText(map[string]any{"schedules": out, "timezone": time.Now().Location().String()}), nil
	case "steve_schedule_cancel":
		var args struct {
			ID string `json:"id"`
		}
		if err := decodeScheduleArgs(raw, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.ID) == "" {
			return "", errors.New("id is required; use the ID returned by steve_schedules")
		}
		job, err := scheduler.CancelSchedule(ctx, b, args.ID)
		if err != nil {
			return "", err
		}
		return jsonText(map[string]any{"cancelled": job.ID}), nil
	default:
		var req ScheduleRequest
		if err := decodeScheduleArgs(raw, &req); err != nil {
			return "", err
		}
		req.Mode, req.When = strings.TrimSpace(req.Mode), strings.Join(strings.Fields(req.When), " ")
		req.Prompt = strings.TrimSpace(req.Prompt)
		req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
		if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 128 {
			return "", errors.New("idempotency_key must contain 1–128 bytes and be reused for retries")
		}
		return s.createSchedule(ctx, b, req, scheduler)
	}
}

type scheduleView struct {
	schedule.Job
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	PendingKey string `json:"pending_key,omitempty"`
}

func describeSchedule(job schedule.Job) scheduleView {
	state := job.State
	if state == "" {
		state = "scheduled"
	}
	return scheduleView{Job: job, State: state, Error: job.Error, PendingKey: job.PendingKey}
}

type scheduleCreation struct {
	Request ScheduleRequest `json:"request"`
	Result  string          `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// The reservation is committed before the store is called. The schedule store
// has no transactional create-with-receipt API: an unfinished reservation must
// remain uncertain across restarts, never be automatically retried.
func (s *Server) createSchedule(ctx context.Context, b Binding, req ScheduleRequest, scheduler Scheduler) (string, error) {
	scope, _ := ScopeFromContext(ctx)
	key := digest(jsonText([]string{b.ConversationID, b.AgentID, scope.TaskID, req.IdempotencyKey}))
	s.mu.Lock()
	store, storeErr := s.store, s.storeErr
	s.mu.Unlock()
	if storeErr != nil {
		return "", storeErr
	}
	if store == nil {
		return "", errors.New("durable scheduling receipts are unavailable")
	}
	record := scheduleCreation{Request: req}
	var exists bool
	err := store.Update(ctx, func(tx StoreTx) error {
		if err := AuthorizeContext(ctx, tx); err != nil {
			return err
		}
		var err error
		exists, err = tx.Get("schedule-create", key, &record)
		if err != nil || exists {
			return err
		}
		if _, err := req.Parse(time.Now()); err != nil {
			return err
		}
		return tx.Put("schedule-create", key, record)
	})
	if err != nil {
		return "", err
	}
	if exists {
		if record.Request != req {
			return "", errors.New("idempotency_key was already used for a different schedule request")
		}
		if record.Result != "" {
			return record.Result, nil
		}
		if record.Error != "" {
			return "", errors.New(record.Error)
		}
		return "", errors.New("schedule creation is in progress or its outcome is unknown; use steve_schedules to inspect it before asking the user to reconcile; do not retry with a new key")
	}
	job, createErr := scheduler.Schedule(ctx, b, req)
	if createErr != nil {
		// Even a failed durable replace may have reached disk before its
		// acknowledgment failed. Replaying this key never creates again.
		record.Error = createErr.Error()
	} else {
		record.Result = jsonText(map[string]any{"schedule": describeSchedule(job), "timezone": time.Now().Location().String()})
	}
	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	if err := store.Update(journalCtx, func(tx StoreTx) error {
		return tx.Put("schedule-create", key, record)
	}); err != nil {
		return "", fmt.Errorf("schedule creation outcome unknown: receipt could not be saved; use steve_schedules before any new request: %w", err)
	}
	return record.Result, createErr
}

func scheduleTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "steve_schedule",
			"description": "Create durable one-shot or recurring work requested by the owner in this private conversation. The server fixes its destination and agent. Reuse idempotency_key for retries. Unattended and delegated tasks cannot create schedules.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"mode":            map[string]any{"type": "string", "enum": []string{"at", "every"}},
					"when":            map[string]any{"type": "string", "description": `Timing only: "30m", "09:00", "day 09:00" or "Mon 09:00" for every; "30m", "tomorrow 09:00" or "2026-10-01 09:00" for at. Server local timezone.`},
					"prompt":          map[string]any{"type": "string", "description": "The work to execute, not another scheduling request. Multiline instructions are preserved."},
					"idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Stable key for this user request within the current task. Reuse with identical arguments on retry; never replace to bypass an uncertain result."},
				},
				"required": []string{"mode", "when", "prompt", "idempotency_key"},
			},
		},
		{
			"name": "steve_schedules", "description": "List this private conversation's schedules for the current project, including next_at, firing state and errors. Does not list other conversations.",
			"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		},
		{
			"name": "steve_schedule_cancel", "description": "Cancel one schedule returned by steve_schedules in this private conversation. Does not undo accepted work. Uncertain firings must be resolved by the user first.",
			"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}},
		},
	}
}

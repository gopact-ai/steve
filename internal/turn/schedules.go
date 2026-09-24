package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

var _ agentmcp.Scheduler = (*Scheduler)(nil)

// Scheduler manages standing work on behalf of an agent's fixed execution.
// It depends only on durable state: it reads attempts, tasks and project
// bindings and creates or deletes stored schedules, keeping no runtime state
// of its own, so it runs apart from any Coordinator.
type Scheduler struct {
	attempts  *attempt.Service
	tasks     *task.Store
	projects  *project.Store
	schedules *schedule.Store
	text      i18n.Catalog
	owners    channelOwners
}

// SchedulerDeps is everything a Scheduler is built with. Attempts, Tasks,
// Projects, Schedules and Text are required; NewScheduler refuses deps
// missing any of them. Owner and ChannelOwners are optional.
type SchedulerDeps struct {
	Attempts  *attempt.Service
	Tasks     *task.Store
	Projects  *project.Store
	Schedules *schedule.Store
	Text      i18n.Catalog
	// Owner is the baseline owner identity; ChannelOwners registers each
	// trusted non-console adapter's native owner.
	Owner         string
	ChannelOwners map[string]string
}

func (d SchedulerDeps) required() []dependency {
	return []dependency{
		{"Attempts", d.Attempts == nil}, {"Tasks", d.Tasks == nil}, {"Projects", d.Projects == nil},
		{"Schedules", d.Schedules == nil}, {"Text", d.Text.IsZero()},
	}
}

// NewScheduler builds a Scheduler from deps, or reports every dependency missing.
func NewScheduler(deps SchedulerDeps) (*Scheduler, error) {
	if missing := absent(deps.required()); len(missing) > 0 {
		return nil, fmt.Errorf("turn: missing schedule dependencies: %s", strings.Join(missing, ", "))
	}
	owners, err := newChannelOwners(deps.Owner, deps.ChannelOwners)
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	return &Scheduler{
		attempts: deps.Attempts, tasks: deps.Tasks, projects: deps.Projects,
		schedules: deps.Schedules, text: deps.Text, owners: owners,
	}, nil
}

// AuthorizeSchedules also gates stored receipt replays in the MCP server.
func (s *Scheduler) AuthorizeSchedules(ctx context.Context, binding agentmcp.Binding, creating bool) error {
	identity, err := s.scheduleIdentity(ctx, binding)
	if err != nil {
		return err
	}
	if creating && identity.Origin != "" {
		return errors.New("unattended work cannot create schedules; execute the stored instruction instead")
	}
	return nil
}

// scheduleIdentity reconstructs the current caller from its fixed execution,
// not the newest attempt or the in-memory conversation mode (lost on restart).
func (s *Scheduler) scheduleIdentity(ctx context.Context, binding agentmcp.Binding) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	if binding.TaskID != "" || binding.DelegatedBy != "" {
		return task.Task{}, errors.New("delegated tasks cannot manage schedules")
	}
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		return task.Task{}, errors.New("scheduling requires an authorized fixed execution")
	}
	r, err := s.attempts.Get(ctx, scope.AttemptID)
	if err != nil {
		return task.Task{}, err
	}
	if r.TaskID != scope.TaskID || r.Agent != binding.AgentID || r.Kind != attempt.KindChat ||
		r.Execution == nil || r.Execution.TaskID != scope.TaskID || r.Execution.Epoch != scope.TaskEpoch ||
		r.Node != scope.NodeID || r.Session != scope.SessionID || attempt.SessionExecutionEpoch(r) != scope.ExecutionGeneration ||
		r.State != attempt.Running || r.Unsettled || r.SessionSettled == nil || *r.SessionSettled || r.SupersededBy != "" {
		return task.Task{}, agentmcp.ErrGrantDenied
	}
	tracked, ok := s.tasks.Get(scope.TaskID)
	if !ok || tracked.Channel != binding.ConversationID || tracked.Member != binding.AgentID || tracked.Parent != "" ||
		!tracked.State.Holds() || tracked.ProjectID != r.Project || tracked.AnchorMessage != r.TurnID {
		return task.Task{}, agentmcp.ErrGrantDenied
	}
	if err := s.tasks.CheckExecution(*r.Execution); err != nil {
		return task.Task{}, err
	}
	if tracked.Transport == "" || tracked.Channel == "" || tracked.AnchorMessage == "" || r.By == "" || r.Project == "" {
		return task.Task{}, errors.New("the current session has no complete schedule identity")
	}
	// Task.Requester belongs to the opening request, whereas By is the sender
	// of this exact turn. A guest's later turn must not inherit the opener's rights.
	tracked.Requester = r.By
	owner, err := s.owners.of(tracked.Transport)
	if err != nil {
		return task.Task{}, err
	}
	if injectionMode(protocol.ChatType(tracked.ChatType), r.By, owner) != home.ModeOwner {
		return task.Task{}, errors.New("scheduling is available only in the owner's private conversation")
	}
	current, ok, err := s.projects.Binding(ctx, tracked.Channel)
	if err != nil {
		return task.Task{}, err
	}
	if !ok || current.ProjectID != r.Project {
		return task.Task{}, errors.New("the current project binding changed; scheduling is refused")
	}
	if err := requireRole(ctx, s.projects, s.text, owner, r.Project, r.By, project.RoleWrite); err != nil {
		return task.Task{}, err
	}
	return tracked, nil
}

// Schedule creates standing work only from a human chat turn. Both one-shot
// and recurring recursion are refused: chaining one-shots is also a loop.
func (s *Scheduler) Schedule(ctx context.Context, binding agentmcp.Binding, req agentmcp.ScheduleRequest) (schedule.Job, error) {
	identity, err := s.scheduleIdentity(ctx, binding)
	if err != nil {
		return schedule.Job{}, err
	}
	if identity.Origin != "" {
		return schedule.Job{}, errors.New("unattended work cannot create schedules; execute the stored instruction instead")
	}
	spec, err := req.Parse(time.Now())
	if err != nil {
		return schedule.Job{}, err
	}
	return s.schedules.Create(schedule.Job{
		Channel: identity.Transport, ProjectID: identity.ProjectID, ConversationID: identity.Channel,
		ChatID: identity.ChatID, ChatType: identity.ChatType, AnchorMessage: identity.AnchorMessage,
		Requester: identity.Requester, Member: identity.Member, Prompt: req.Prompt, Spec: spec,
	})
}

func scheduleVisible(job schedule.Job, identity task.Task) bool {
	return job.ConversationID == identity.Channel && job.Channel == identity.Transport && job.ProjectID == identity.ProjectID
}

// Schedules only exposes jobs in this execution's conversation and project.
func (s *Scheduler) Schedules(ctx context.Context, binding agentmcp.Binding) ([]schedule.Job, error) {
	identity, err := s.scheduleIdentity(ctx, binding)
	if err != nil {
		return nil, err
	}
	out := []schedule.Job{}
	for _, job := range s.schedules.List(identity.Channel) {
		if scheduleVisible(job, identity) {
			out = append(out, job)
		}
	}
	return out, nil
}

// CancelSchedule keeps the durable store's unresolved-firing protection.
func (s *Scheduler) CancelSchedule(ctx context.Context, binding agentmcp.Binding, id string) (schedule.Job, error) {
	identity, err := s.scheduleIdentity(ctx, binding)
	if err != nil {
		return schedule.Job{}, err
	}
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	job, ok := s.schedules.Get(id)
	if !ok || !scheduleVisible(job, identity) {
		return schedule.Job{}, fmt.Errorf("schedule %q was not found in this conversation and project", id)
	}
	removed, ok, err := s.schedules.Delete(job.ID)
	if err != nil {
		return schedule.Job{}, err
	}
	if !ok {
		return schedule.Job{}, fmt.Errorf("schedule %q was not found", id)
	}
	return removed, nil
}

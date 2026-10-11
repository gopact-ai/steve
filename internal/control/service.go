// Package control owns authorized platform use cases. A conversation address
// identifies a resource; it never supplies the caller's project or owner role.
package control

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
	"github.com/gopact-ai/steve/internal/work"
)

var ErrDenied = identity.ErrDenied

type Service struct {
	Owners                      identity.Owners
	Projects                    *project.Store
	Tasks                       *task.Store
	Schedules                   *schedule.Store
	Plans                       *plan.Store
	DefaultProject, HomeProject string
}

func (s *Service) book() *ledger.Ledger {
	if accounts := s.Owners.Configuration().Accounts; accounts != nil {
		return accounts.Book
	}
	return nil
}
func channel(req turnapi.Request) string {
	if req.Source.Channel == "" {
		return "console"
	}
	return req.Source.Channel
}

// CallerTx checks the immutable accepted input, including revocation between
// admission and a control's actual state mutation. Trusted internal consumers
// without durable account configuration keep their existing owner namespace.
func (s *Service) CallerTx(tx ledger.Reader, req turnapi.Request) (string, string, error) {
	if s.book() == nil {
		return s.Owners.Resolve(context.Background(), req.Source.Channel, req.Actor.ID)
	}
	account := identity.Account{Channel: channel(req), ID: req.Actor.ID}
	binding, err := identity.ResolveAccount(tx, account)
	if err != nil {
		return "", "", err
	}
	if req.Source.MessageID == "" {
		return "", "", ErrDenied
	}
	input, found, err := durable.Get[work.InputRecord](tx, work.InputKind, durable.Key(channel(req), req.Source.ConversationID, req.Source.MessageID))
	if err != nil {
		return "", "", err
	}
	if !found || input.Actor != account || input.Address != req.Address() || input.ChatType != string(req.Source.ChatType) || input.State != work.InputRunning || input.Principal != binding.Principal || input.AccountRevision != binding.Revision {
		return "", "", ErrDenied
	}
	route, found, err := durable.Get[conversation.Route](tx, conversation.RouteKind, req.Source.ConversationID)
	if err != nil {
		return "", "", err
	}
	if !found || route.Channel != channel(req) || route.Conversation != req.Source.ConversationID {
		return "", "", ErrDenied
	}
	return binding.Principal, s.Owners.Configuration().Accounts.Owner, nil
}

func (s *Service) CheckProjectTx(tx ledger.Reader, req turnapi.Request, id string, want project.Role) error {
	principal, owner, err := s.CallerTx(tx, req)
	if err != nil {
		return err
	}
	if principal != "" && owner != "" && principal == owner {
		return nil
	}
	if id == "" {
		return ErrDenied
	}
	role, err := project.AccessPolicyTx(tx, id, principal, owner)
	if err != nil {
		return err
	}
	if !role.AtLeast(want) {
		return ErrDenied
	}
	return nil
}

func (s *Service) projectTx(tx ledger.Reader, req turnapi.Request) (string, error) {
	b, found, err := durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
	if err != nil {
		return "", err
	}
	if found {
		return b.ProjectID, nil
	}
	principal, owner, err := s.CallerTx(tx, req)
	if err != nil {
		return "", err
	}
	if req.Source.ChatType == protocol.ChatP2P && owner != "" && principal == owner && s.HomeProject != "" {
		return s.HomeProject, nil
	}
	return s.DefaultProject, nil
}
func (s *Service) OwnerTx(tx ledger.Reader, req turnapi.Request, private bool) error {
	principal, owner, err := s.CallerTx(tx, req)
	if err != nil {
		return err
	}
	if owner == "" || principal != owner || private && req.Source.ChatType != protocol.ChatP2P {
		return ErrDenied
	}
	return nil
}

// Guard is evaluated by the store in the same transaction as its mutation.
func (s *Service) Guard(req turnapi.Request, want project.Role) func(*ledger.Tx) error {
	if s.book() == nil {
		return nil
	}
	var expected project.Binding
	var bound bool
	err := s.book().Read(context.Background(), func(tx *ledger.ReadTx) error {
		var err error
		expected, bound, err = durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
		return err
	})
	return func(tx *ledger.Tx) error {
		if err != nil {
			return err
		}
		current, found, err := durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
		if err != nil {
			return err
		}
		if found != bound || found && current != expected {
			return ledger.ErrConflict
		}
		id, err := s.projectTx(tx, req)
		if err != nil {
			return err
		}
		return s.CheckProjectTx(tx, req, id, want)
	}
}

func (s *Service) CreateSchedule(ctx context.Context, req turnapi.Request, job schedule.Job) (schedule.Job, error) {
	return s.Schedules.CreateChecked(ctx, job, s.ScheduleGuard(req))
}

func (s *Service) CreateTask(ctx context.Context, req turnapi.Request, input task.Task) (task.Task, error) {
	var guard func(*ledger.Tx) error
	if s.book() != nil {
		guard = func(tx *ledger.Tx) error {
			if input.Transport != channel(req) || input.Channel != req.Source.ConversationID || input.Requester != req.Actor.ID {
				return ErrDenied
			}
			return s.CheckProjectTx(tx, req, input.ProjectID, project.RoleWrite)
		}
	}
	return s.Tasks.CreateChecked(ctx, input, guard)
}

func (s *Service) ListSchedules(ctx context.Context, req turnapi.Request) ([]schedule.Job, error) {
	if err := s.Check(ctx, req, project.RoleRead); err != nil {
		return nil, err
	}
	var out []schedule.Job
	for _, job := range s.Schedules.List(req.Source.ConversationID) {
		if job.Channel != req.Source.Channel {
			continue
		}
		if book := s.book(); book != nil {
			err := book.Read(ctx, func(tx *ledger.ReadTx) error { return s.CheckProjectTx(tx, req, job.ProjectID, project.RoleRead) })
			if errors.Is(err, ErrDenied) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		out = append(out, job)
	}
	return out, nil
}

func (s *Service) ProjectsFor(ctx context.Context, req turnapi.Request) ([]project.Project, error) {
	all, err := s.Projects.List(ctx)
	if err != nil {
		return nil, err
	}
	if book := s.book(); book != nil {
		visible := make([]project.Project, 0, len(all))
		err = book.Read(ctx, func(tx *ledger.ReadTx) error {
			for _, p := range all {
				if err := s.CheckProjectTx(tx, req, p.ID, project.RoleRead); errors.Is(err, ErrDenied) {
					continue
				} else if err != nil {
					return err
				}
				visible = append(visible, p)
			}
			return nil
		})
		return visible, err
	}
	return all, nil
}

func (s *Service) BindProject(ctx context.Context, req turnapi.Request, id string, initial bool) (project.Binding, error) {
	var guard func(*ledger.Tx) error
	if s.book() != nil {
		guard = func(tx *ledger.Tx) error {
			b, found, err := durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
			if err != nil {
				return err
			}
			if found {
				if initial {
					return ledger.ErrConflict
				}
				if err := s.CheckProjectTx(tx, req, b.ProjectID, project.RoleWrite); err != nil {
					return err
				}
			}
			want := project.RoleRead
			if initial {
				want = project.RoleWrite
			}
			return s.CheckProjectTx(tx, req, id, want)
		}
	}
	by := req.Actor.ID
	if initial {
		by = "default"
	}
	return s.Projects.BindChecked(ctx, req.Source.ConversationID, id, by, guard)
}

// BoundGuard pins a continuation of a successful switch to its resulting
// binding; archive writes cannot silently act on a subsequent project switch.
func (s *Service) BoundGuard(req turnapi.Request, expected project.Binding, want project.Role) func(*ledger.Tx) error {
	if s.book() == nil {
		return nil
	}
	return func(tx *ledger.Tx) error {
		b, found, err := durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
		if err != nil {
			return err
		}
		if !found || b != expected {
			return ledger.ErrConflict
		}
		return s.CheckProjectTx(tx, req, expected.ProjectID, want)
	}
}
func (s *Service) TaskGuard(req turnapi.Request, id string, want project.Role) func(*ledger.Tx) error {
	if s.book() == nil {
		return nil
	}
	return func(tx *ledger.Tx) error {
		tracked, found, err := task.GetTx(tx, id)
		if err != nil {
			return err
		}
		if !found || tracked.Transport != channel(req) || tracked.Channel == "" || tracked.Channel != req.Source.ConversationID {
			return ErrDenied
		}
		return s.CheckProjectTx(tx, req, tracked.ProjectID, want)
	}
}
func (s *Service) Check(ctx context.Context, req turnapi.Request, want project.Role) error {
	if book := s.book(); book != nil {
		return book.Update(ctx, s.Guard(req, want))
	}
	return nil
}
func (s *Service) Owner(ctx context.Context, req turnapi.Request, private bool) error {
	if book := s.book(); book != nil {
		return book.Update(ctx, func(tx *ledger.Tx) error { return s.OwnerTx(tx, req, private) })
	}
	if !s.Owners.IsOwner(ctx, req.Source.Channel, req.Actor.ID) || private && req.Source.ChatType != protocol.ChatP2P {
		return ErrDenied
	}
	return nil
}

func (s *Service) OwnerGuard(req turnapi.Request) func(*ledger.Tx) error {
	if s.book() == nil {
		return nil
	}
	return func(tx *ledger.Tx) error { return s.OwnerTx(tx, req, true) }
}

func (s *Service) Task(ctx context.Context, req turnapi.Request, id string, want project.Role) (task.Task, error) {
	tracked, found := s.Tasks.Get(id)
	if !found || tracked.Transport != req.Source.Channel || tracked.Channel == "" || tracked.Channel != req.Source.ConversationID {
		return task.Task{}, ErrDenied
	}
	if book := s.book(); book != nil {
		if err := book.Update(ctx, s.TaskGuard(req, id, want)); err != nil {
			return task.Task{}, err
		}
	}
	return tracked, nil
}
func (s *Service) ListTasks(ctx context.Context, req turnapi.Request) ([]task.Task, error) {
	if err := s.Check(ctx, req, project.RoleRead); err != nil {
		return nil, err
	}
	var out []task.Task
	for _, tracked := range s.Tasks.List(req.Source.ConversationID) {
		if tracked.Transport == req.Source.Channel {
			if _, err := s.Task(ctx, req, tracked.ID, project.RoleRead); err == nil {
				out = append(out, tracked)
			} else if !errors.Is(err, ErrDenied) {
				return nil, err
			}
		}
	}
	return out, nil
}
func (s *Service) Settle(ctx context.Context, req turnapi.Request, id string, as task.Settlement) (task.Task, error) {
	tracked, err := s.SettlementTask(ctx, req, id)
	if err != nil {
		return task.Task{}, err
	}
	guard := s.TaskGuard(req, id, project.RoleWrite)
	if backgroundPlan(tracked) {
		guard = s.backgroundSettlementGuard(req, id)
	}
	return s.Tasks.SettleChecked(ctx, id, as, guard)
}

func backgroundPlan(tracked task.Task) bool {
	return tracked.Channel == "" && tracked.Parent == "" && tracked.Origin == "plan"
}

// SettlementTask permits central owner reconciliation of an unaddressed
// background plan. It does not assign the task to this chat or let a chat
// continue its execution. Every addressed task retains its source policy.
func (s *Service) SettlementTask(ctx context.Context, req turnapi.Request, id string) (task.Task, error) {
	tracked, found := s.Tasks.Get(id)
	if !found || !backgroundPlan(tracked) {
		return s.Task(ctx, req, id, project.RoleWrite)
	}
	if req.Source.Channel != "console" {
		return task.Task{}, ErrDenied
	}
	if book := s.book(); book != nil {
		if err := book.Update(ctx, s.backgroundSettlementGuard(req, id)); err != nil {
			return task.Task{}, err
		}
	} else if err := s.Owner(ctx, req, true); err != nil {
		return task.Task{}, err
	}
	return tracked, nil
}

func (s *Service) backgroundSettlementGuard(req turnapi.Request, id string) func(*ledger.Tx) error {
	if s.book() == nil {
		return nil
	}
	return func(tx *ledger.Tx) error {
		tracked, found, err := task.GetTx(tx, id)
		if err != nil {
			return err
		}
		if !found || !backgroundPlan(tracked) || req.Source.Channel != "console" {
			return ErrDenied
		}
		return s.OwnerTx(tx, req, true)
	}
}
func (s *Service) ScheduleGuard(req turnapi.Request) func(*ledger.Tx, schedule.Job) error {
	if s.book() == nil {
		return nil
	}
	return func(tx *ledger.Tx, job schedule.Job) error {
		if job.Channel != channel(req) || job.ConversationID != req.Source.ConversationID {
			return ErrDenied
		}
		if err := s.CheckProjectTx(tx, req, job.ProjectID, project.RoleWrite); err != nil {
			return err
		}
		caller, owner, err := s.CallerTx(tx, req)
		if err != nil {
			return err
		}
		if caller == owner && owner != "" {
			return nil
		}
		original, err := identity.ResolveAccount(tx, identity.Account{Channel: job.Channel, ID: job.Requester})
		if err != nil {
			return err
		}
		if caller != original.Principal {
			return ErrDenied
		}
		return nil
	}
}
func (s *Service) CancelSchedule(ctx context.Context, req turnapi.Request, id string) (schedule.Job, bool, error) {
	job, found := s.Schedules.Get(id)
	if !found || job.Channel != req.Source.Channel || job.ConversationID != req.Source.ConversationID {
		return schedule.Job{}, false, ErrDenied
	}
	return s.Schedules.DeleteChecked(ctx, id, s.ScheduleGuard(req))
}
func (s *Service) ResolveSchedule(ctx context.Context, req turnapi.Request, id, verdict string) error {
	return s.Schedules.ResolveFiringChecked(ctx, id, verdict, req.Actor.ID, s.ScheduleGuard(req))
}

type guardKey struct{}
type requestKey struct{}

func WithRequest(ctx context.Context, req turnapi.Request) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

func RequestFromContext(ctx context.Context) (turnapi.Request, bool) {
	req, ok := ctx.Value(requestKey{}).(turnapi.Request)
	return req, ok
}

func (s *Service) TaskContextGuard(ctx context.Context, id string) func(*ledger.Tx) error {
	if req, ok := RequestFromContext(ctx); ok {
		return s.TaskGuard(req, id, project.RoleWrite)
	}
	return MutationGuard(ctx)
}

// WithMutationGuard carries a source policy to the existing native control
// ports; a context value does not replace any execution/lease precondition.
func WithMutationGuard(ctx context.Context, guard func(*ledger.Tx) error) context.Context {
	return context.WithValue(ctx, guardKey{}, guard)
}
func MutationGuard(ctx context.Context) func(*ledger.Tx) error {
	guard, _ := ctx.Value(guardKey{}).(func(*ledger.Tx) error)
	return guard
}
func (s *Service) Resume(ctx context.Context, req turnapi.Request, tracked task.Task, admission task.ResumeAdmission) (task.Task, error) {
	if _, err := s.Task(ctx, req, tracked.ID, project.RoleWrite); err != nil {
		return task.Task{}, err
	}
	return s.Tasks.ResumeChecked(ctx, tracked.ID, tracked.ExecutionEpoch, tracked.State, admission, s.TaskGuard(req, tracked.ID, project.RoleWrite))
}
func (s *Service) PlansFor(ctx context.Context, req turnapi.Request, id string) ([]plan.Plan, error) {
	if err := s.Check(ctx, req, project.RoleRead); err != nil {
		return nil, err
	}
	var candidates []plan.Plan
	if id != "" {
		p, found := s.Plans.Latest(id)
		if !found {
			return nil, nil
		}
		candidates = []plan.Plan{p}
	} else {
		candidates = s.Plans.List()
	}
	var out []plan.Plan
	for _, p := range candidates {
		if _, err := s.Task(ctx, req, p.TaskID, project.RoleRead); err == nil {
			out = append(out, p)
		} else if !errors.Is(err, ErrDenied) {
			return nil, err
		}
	}
	return out, nil
}

// CheckCommand checks the policy before agent selection mutates a conversation.
// The selected business operation checks again at its own commit boundary.
func (s *Service) CheckCommand(ctx context.Context, req turnapi.Request, cmd protocol.Command, rest string, switchOnly bool) error {
	if s.book() == nil {
		return nil
	}
	switch cmd {
	case protocol.CommandSkills, protocol.CommandRepair, protocol.CommandEffects, protocol.CommandApprove, protocol.CommandDeny:
		return s.Owner(ctx, req, true)
	case protocol.CommandFleet:
		if strings.TrimSpace(rest) == "probe" {
			return s.Owner(ctx, req, true)
		}
		return s.Check(ctx, req, project.RoleRead)
	case protocol.CommandGrant:
		fields := strings.Fields(rest)
		if len(fields) == 1 || len(fields) == 3 {
			return s.book().Update(ctx, func(tx *ledger.Tx) error { return s.CheckProjectTx(tx, req, fields[0], project.RoleAdmin) })
		}
		return nil
	case protocol.CommandStatus, protocol.CommandPlans:
		return s.Check(ctx, req, project.RoleRead)
	case protocol.CommandTasks:
		verb, _, _ := protocol.ParseTaskArgs(rest)
		if verb == protocol.TaskList || verb == protocol.TaskShow {
			return s.Check(ctx, req, project.RoleRead)
		}
	case protocol.CommandSchedules:
		verb, _, _ := protocol.ParseTaskArgs(rest)
		if verb == protocol.TaskList || verb == protocol.TaskShow {
			return s.Check(ctx, req, project.RoleRead)
		}
	case protocol.CommandHistory:
		if strings.TrimSpace(rest) == "" {
			return s.Check(ctx, req, project.RoleRead)
		}
	case protocol.CommandProject:
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return s.Check(ctx, req, project.RoleRead)
		}
		if len(fields) == 2 && fields[0] == "use" {
			return s.book().Update(ctx, func(tx *ledger.Tx) error {
				b, found, err := durable.Get[project.Binding](tx, "conversation-project", req.Source.ConversationID)
				if err != nil {
					return err
				}
				if found {
					if err := s.CheckProjectTx(tx, req, b.ProjectID, project.RoleWrite); err != nil {
						return err
					}
				}
				return s.CheckProjectTx(tx, req, fields[1], project.RoleRead)
			})
		}
	}
	if cmd != "" || switchOnly {
		return s.Check(ctx, req, project.RoleWrite)
	}
	return nil // Ordinary execution performs its existing project admission.
}

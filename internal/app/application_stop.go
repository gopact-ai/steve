package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

type applicationStopSessions interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
}

type applicationOpenRecovery interface {
	ReconcileNodeOpen(context.Context, harness.Placement, string, bool) (nodewire.SessionState, error)
}

type applicationStops struct {
	mu       sync.Mutex
	attempts *attempt.Service
	tasks    *task.Store
	sessions applicationStopSessions
	after    string
}

func newApplicationStops(attempts *attempt.Service, tasks *task.Store, sessions applicationStopSessions) *applicationStops {
	return &applicationStops{attempts: attempts, tasks: tasks, sessions: sessions}
}

// Reconcile consumes SetAside's durable revocation of original task tokens.
// It never admits an execution or sends Prompt. A small rotating batch keeps
// offline nodes from starving later stops; each pass has a bounded lifetime.
func (s *applicationStops) Reconcile(parent context.Context) error {
	if !s.mu.TryLock() {
		return nil
	}
	defer s.mu.Unlock()
	if s.attempts == nil || s.tasks == nil || s.sessions == nil {
		return errors.New("durable task stopping is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	live, err := s.attempts.Live(ctx)
	if err != nil {
		return err
	}
	closed, err := s.attempts.Closed(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var pending []attempt.Record
	for _, r := range append(live, closed...) {
		if seen[r.ID] || r.State == attempt.Superseded || (!strings.HasPrefix(r.Session, "ns_") && !attempt.PendingSessionOpen(r)) || r.Node == "" || r.Execution == nil {
			continue
		}
		seen[r.ID] = true
		if r.State.Terminal() && !r.Unsettled && r.SessionSettled != nil && *r.SessionSettled && (r.StopEvidence != "task-stop/"+r.ID || !s.accountingPending(r)) {
			continue
		}
		if _, ok := s.tasks.Get(r.TaskID); ok && errors.Is(s.tasks.CheckExecution(*r.Execution), task.ErrExecutionStopped) {
			pending = append(pending, r)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) == 0 {
		return nil
	}
	start := sort.Search(len(pending), func(i int) bool { return pending[i].ID > s.after })
	if start == len(pending) {
		start = 0
	}
	count := min(len(pending), 4)
	failures := make(chan error, count)
	for index := range count {
		r := pending[(start+index)%len(pending)]
		s.after = r.ID
		go func() { failures <- s.stop(ctx, r) }()
	}
	var result error
	for range count {
		result = errors.Join(result, <-failures)
	}
	return result
}

func (s *applicationStops) stop(parent context.Context, r attempt.Record) error {
	ctx, cancel := context.WithTimeout(parent, 17*time.Second)
	defer cancel()
	if !errors.Is(s.tasks.CheckExecution(*r.Execution), task.ErrExecutionStopped) {
		return nil
	}
	if r.StopEvidence == "task-stop/"+r.ID && r.SessionSettled != nil && *r.SessionSettled && !r.Unsettled {
		return s.projectStopped(r)
	}
	tracked, ok := s.tasks.Get(r.TaskID)
	if !ok {
		return errors.New("stopped task source is no longer available")
	}
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: r.TaskID, InstanceID: r.TurnID, AttemptID: r.ID})
	failed := func(cause error) error {
		if parent.Err() != nil {
			return cause
		}
		message := "暂停或取消已记录，但尚未收到原节点的停止确认。已尝试联系原执行；节点恢复后会继续核对并停止同一次执行。"
		err := s.attempts.TaskStopPending(parent, r.ID, "task-stop-recovery", message)
		if err != nil {
			return errors.Join(fmt.Errorf("task %s attempt %s on %s: native stop remains pending: %w", r.TaskID, r.ID, r.Node, cause), err)
		}
		if !r.Unsettled || r.Error != message {
			slog.Warn(fmt.Sprintf("steve: native stop pending task=%s attempt=%s node=%s: %v", r.TaskID, r.ID, r.Node, cause), "task", r.TaskID, "attempt", r.ID, "node", r.Node)
		}
		return nil
	}
	if attempt.PendingSessionOpen(r) {
		recovery, ok := s.sessions.(applicationOpenRecovery)
		if !ok {
			return failed(errors.New("node cannot reconcile the original session open"))
		}
		state, err := recovery.ReconcileNodeOpen(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Workspace.Path, true)
		if err != nil {
			return failed(err)
		}
		stopped, err := s.attempts.ConfirmTaskStopped(ctx, r.ID, "task-stop-recovery", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: state})
		if err != nil {
			return failed(err)
		}
		return s.projectStopped(stopped)
	}
	runner, err := s.sessions.AttachRetainedSession(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Session, r.Workspace.Path)
	if err != nil {
		return failed(err)
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return failed(errors.New("node session cannot inspect its original input"))
	}
	state, err := inspector.InspectRetained(ctx)
	if err != nil {
		return failed(err)
	}
	expected := nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}
	if state.ID != r.Session || state.Harness != r.Harness || state.Binding != expected || state.Command != nil && state.Command.ID != attempt.InputCommandID(r) {
		return failed(errors.New("node observation belongs to another execution"))
	}
	stopper, ok := runner.(harness.RetainedStopper)
	if !ok {
		return failed(errors.New("node cannot stop the retained execution"))
	}
	state, err = stopper.StopRetained(ctx)
	if err != nil {
		return failed(err)
	}
	stopped, err := s.attempts.ConfirmTaskStopped(ctx, r.ID, "task-stop-recovery", attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: state})
	if err != nil {
		return failed(err)
	}
	return s.projectStopped(stopped)
}

func stoppedAccounting(r attempt.Record) task.RecoveryUsage {
	if r.Usage == nil {
		return task.RecoveryUsage{}
	}
	u := r.Usage
	return task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}, Model: u.Model, Reported: u.Reported}
}

func (s *applicationStops) projectStopped(r attempt.Record) error {
	if err := s.tasks.SettleAttempt(r.TaskID, r.ID, r.TurnID, r.EndedAt, task.OutcomeCancelled, stoppedAccounting(r)); err != nil {
		return fmt.Errorf("task %s attempt %s: native stop confirmed; original usage accounting remains pending: %w", r.TaskID, r.ID, err)
	}
	return nil
}

func (s *applicationStops) accountingPending(r attempt.Record) bool {
	tracked, ok := s.tasks.Get(r.TaskID)
	if !ok {
		return true
	}
	u := stoppedAccounting(r)
	for _, row := range tracked.Attempts {
		if row.ExecutionID != r.ID || row.TurnID != r.TurnID {
			continue
		}
		if row.Open() || row.UsageKnown == nil {
			return true
		}
		known := u.Reported || u.Tokens.Input != 0 || u.Tokens.Output != 0 || u.Tokens.CachedRead != 0 || u.Tokens.CachedWrite != 0
		return known && (!*row.UsageKnown || row.Tokens != u.Tokens || row.Model != u.Model)
	}
	return true
}

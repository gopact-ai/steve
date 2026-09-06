package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	gopact "github.com/gopact-ai/gopact"
	"github.com/gopact-ai/gopact/runlog"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

const (
	RunExecuting = "executing"
	RunLanding   = "landing"
	RunCompleted = "completed"
	runKind      = "plan-run"
)

type Sink struct {
	StepID    string           `json:"step_id"`
	Artifact  string           `json:"artifact"`
	LandingID string           `json:"landing_id"`
	Source    *artifact.Source `json:"source,omitempty"`
}

// RunRecord keeps execution responsibility until all promised landings and
// task completion are durable. Completed records remain replayable facts.
type RunRecord struct {
	Target    project.Home `json:"target"`
	fresh     bool
	Base      string               `json:"base"`
	ID        string               `json:"id"`
	PlanID    string               `json:"plan_id"`
	Rev       int                  `json:"rev"`
	TaskID    string               `json:"task_id"`
	ProjectID string               `json:"project_id"`
	RunID     string               `json:"run_id"`
	Phase     string               `json:"phase"`
	Outcome   string               `json:"outcome,omitempty"`
	Error     string               `json:"error,omitempty"`
	Execution *task.ExecutionToken `json:"execution,omitempty"`
	Sinks     []Sink               `json:"sinks,omitempty"`
	OpenedAt  time.Time            `json:"opened_at"`
	Owner     string               `json:"owner"`
}

func runIDFor(p plan.Plan) string                              { return fmt.Sprintf("plan-%s-r%d", p.ID, p.Rev) }
func planRunID(id string) string                               { return "plan-run/" + id }
func (s *Supervisor) SetLedger(l *ledger.Ledger, owner string) { s.ledger, s.owner = l, owner }
func (s *Supervisor) SetTasks(tasks *task.Store)               { s.tasks = tasks }

// PrepareRecovery is startup-only, before this hub admits new executions.
func (s *Supervisor) PrepareRecovery(ctx context.Context) error {
	if s.ledger == nil {
		return errors.New("plan run ledger is not configured")
	}
	open, err := s.OpenRuns(ctx)
	if err != nil {
		return err
	}
	for _, rec := range open {
		if err := s.ledger.Invalidate(ctx, "plan-driver:"+rec.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) loadRun(ctx context.Context, id string) (RunRecord, bool, error) {
	op, found, err := s.ledger.Operation(ctx, id)
	if err != nil || !found {
		return RunRecord{}, found, err
	}
	if op.Kind != runKind {
		return RunRecord{}, false, errors.New("plan run identity belongs to another operation")
	}
	var rec RunRecord
	if err := json.Unmarshal(op.Data, &rec); err != nil {
		return rec, false, err
	}
	if rec.ID != id || rec.PlanID == "" {
		return rec, false, errors.New("invalid plan run identity")
	}
	rec.Phase = op.State
	return rec, true, nil
}

func (s *Supervisor) opened(ctx context.Context, p plan.Plan) (RunRecord, error) {
	if s.ledger == nil {
		return RunRecord{}, errors.New("plan execution requires a durable ledger")
	}
	id := planRunID(p.ID)
	if rec, found, err := s.loadRun(ctx, id); err != nil || found {
		return rec, err
	}
	if s.deps.Artifacts == nil {
		return RunRecord{}, errors.New("plan artifacts are not configured")
	}
	proj, found, err := s.deps.Artifacts.Project(ctx, p.ProjectID)
	if err != nil {
		return RunRecord{}, err
	}
	if !found {
		return RunRecord{}, errors.New("plan project is missing")
	}
	rec := RunRecord{Target: proj.Home, ID: id, PlanID: p.ID, Rev: p.Rev, TaskID: p.TaskID, ProjectID: p.ProjectID, Base: p.Base, RunID: runIDFor(p), Phase: RunExecuting, Execution: execution.Token(ctx), OpenedAt: time.Now().UTC(), Owner: s.owner}
	if rec.Execution == nil && s.tasks != nil && p.TaskID != "" {
		token, err := s.tasks.ExecutionToken(p.TaskID)
		if err != nil {
			return rec, err
		}
		rec.Execution = &token
	}
	_, err = s.ledger.Begin(ctx, id, runKind, RunExecuting, s.owner, rec)
	rec.fresh = err == nil
	return rec, err
}

func (s *Supervisor) prepareBase(ctx context.Context, rec *RunRecord, p *plan.Plan) error {
	if rec.Base == "" {
		proj, found, err := s.deps.Artifacts.Project(ctx, p.ProjectID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("plan project is missing")
		}
		base, _, err := s.deps.Artifacts.SnapshotCanonical(ctx, proj, s.deps.Artifacts.CanonicalOf(ctx, proj.ID), rec.ID, "base of plan "+p.ID)
		if err != nil {
			return err
		}
		rec.Base = base.ID
		if err := s.saveRun(ctx, rec, RunExecuting); err != nil {
			return err
		}
	}
	p.Base = rec.Base
	return nil
}

func (s *Supervisor) saveRun(ctx context.Context, rec *RunRecord, phase string) error {
	from := rec.Phase
	next := *rec
	next.Phase = phase
	_, err := s.ledger.Transition(ctx, rec.ID, from, phase, s.owner, runFence(ctx), nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		if phase != RunCompleted {
			if err := task.CheckExecutionTx(tx, next.Execution); err != nil {
				return err
			}
		}
		return tx.SetData(op, next)
	})
	if err == nil {
		*rec = next
	}
	return err
}

func (s *Supervisor) OpenRuns(ctx context.Context) ([]RunRecord, error) {
	if s.ledger == nil {
		return nil, errors.New("plan run ledger is not configured")
	}
	ops, err := s.ledger.Operations(ctx, runKind, "")
	if err != nil {
		return nil, err
	}
	out := []RunRecord{}
	for _, op := range ops {
		if op.State == RunCompleted {
			continue
		}
		var rec RunRecord
		if err := json.Unmarshal(op.Data, &rec); err != nil {
			return out, err
		}
		rec.Phase = op.State
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out, nil
}

func (s *Supervisor) runOwner(ctx context.Context, rec RunRecord, work func(context.Context, RunRecord) (Outcome, error)) (Outcome, error) {
	project, found, err := s.deps.Artifacts.Project(ctx, rec.ProjectID)
	if err != nil {
		return Outcome{}, err
	}
	if !found || rec.Target.Path == "" || project.Home != rec.Target {
		return Outcome{}, fmt.Errorf("%w: plan %s target changed", ErrRecovery, rec.PlanID)
	}
	if s.deps.Executions != nil && rec.Execution != nil {
		scope, err := s.deps.Executions.BeginAccepted(ctx, execution.Key{TaskID: rec.TaskID, InstanceID: rec.ID}, rec.Execution)
		if err != nil {
			return Outcome{}, err
		}
		defer scope.Finish(nil)
		ctx = scope.Context()
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return task.CheckExecutionTx(tx, rec.Execution) }); err != nil {
		return Outcome{}, err
	}
	// A distinct driver token prevents two callers from scheduling the same
	// plan concurrently, even when they use different Supervisor instances.
	ttl := s.driverTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	driver, err := s.ledger.Acquire(ctx, "plan-driver:"+rec.ID, fmt.Sprintf("%s/%d", s.owner, time.Now().UnixNano()), ttl)
	if err != nil {
		return Outcome{}, err
	}
	ctx, stopDriver := s.keepRunDriver(ctx, driver, ttl)
	defer stopDriver()
	fresh, _, err := s.loadRun(ctx, rec.ID)
	if err != nil {
		return Outcome{}, err
	}
	return work(ctx, fresh)
}

// Resume preserves the original authorization. Paused or cancelled epochs
// never authorize new work or landings after a user later resumes the task.
func (s *Supervisor) Resume(ctx context.Context, rec RunRecord) (Outcome, error) {
	if s.ledger == nil {
		return Outcome{}, errors.New("plan run ledger is not configured")
	}
	return s.runOwner(ctx, rec, func(ctx context.Context, rec RunRecord) (Outcome, error) {
		if s.plans == nil {
			return Outcome{}, errors.New("plan store is not configured")
		}
		p, ok := s.plans.Latest(rec.PlanID)
		if !ok {
			return Outcome{}, fmt.Errorf("plan %s vanished", rec.PlanID)
		}
		if rec.Phase == RunLanding || rec.Phase == RunCompleted {
			return s.finishRun(ctx, rec, p, Outcome{RunID: rec.RunID})
		}
		if p.Rev != rec.Rev {
			rec.Rev, rec.RunID = p.Rev, runIDFor(p)
			if err := s.saveRun(ctx, &rec, RunExecuting); err != nil {
				return Outcome{}, err
			}
		}
		if err := s.prepareBase(ctx, &rec, &p); err != nil {
			return Outcome{}, err
		}
		out, err := s.runs.Resume(ctx, p, s.deps, rec.RunID)
		if err != nil && ctx.Err() == nil {
			out, err = s.continueFrom(ctx, p, out, err)
		}
		return s.executed(ctx, rec, p, out, err)
	})
}

func (s *Supervisor) executed(ctx context.Context, rec RunRecord, p plan.Plan, out Outcome, runErr error) (Outcome, error) {
	if runErr == nil {
		runErr = ctx.Err()
	}
	if runErr != nil {
		if errors.Is(runErr, ErrProjection) || errors.Is(runErr, ErrRecovery) || errors.Is(runErr, ErrCompletion) {
			return out, runErr
		}
		var exhausted ErrExhausted
		var nowhere ErrNowhereToRun
		var noBudget ErrNoBudget
		if errors.As(runErr, &exhausted) || errors.As(runErr, &nowhere) || errors.As(runErr, &noBudget) {
			rec.Outcome, rec.Error = "failed", runErr.Error()
			if err := s.saveRun(ctx, &rec, RunCompleted); err != nil {
				return out, errors.Join(runErr, err)
			}
			return out, runErr
		}
		// Store/cancellation/uncertain completion failures remain resumable.
		if ctx.Err() == nil && !errors.Is(runErr, ErrProjection) && !errors.Is(runErr, ErrRecovery) && !errors.Is(runErr, ErrCompletion) && !errors.Is(runErr, task.ErrExecutionStopped) {
			rec.Error = runErr.Error()
			if err := s.saveRun(ctx, &rec, RunExecuting); err != nil {
				return out, errors.Join(runErr, err)
			}
		}
		return out, runErr
	}
	if s.plans != nil {
		if current, ok := s.plans.Latest(p.ID); ok {
			p = current
		}
	}
	p.Base = rec.Base
	rec.Rev, rec.RunID = p.Rev, out.RunID
	if rec.RunID == "" {
		rec.RunID = runIDFor(p)
	}
	sinks, err := runSinks(p, out)
	if err != nil {
		return out, err
	}
	kept := sinks[:0]
	for _, sink := range sinks {
		manifest, found, err := s.deps.Artifacts.Manifest(ctx, sink.Artifact)
		if err != nil {
			return out, err
		}
		if !found {
			return out, fmt.Errorf("sink artifact %s is missing", sink.Artifact)
		}
		if !manifest.Canonical {
			kept = append(kept, sink)
		}
	}
	sinks = kept
	rec.Sinks, rec.Error = sinks, ""
	if err := s.saveRun(ctx, &rec, RunLanding); err != nil {
		return out, err
	}
	return s.finishRun(ctx, rec, p, out)
}

func runSinks(p plan.Plan, out Outcome) ([]Sink, error) {
	depended := map[string]bool{}
	for _, step := range p.Steps {
		for _, id := range dependencies(step) {
			depended[id] = true
		}
	}
	results := map[string]plan.StepResult{}
	for _, r := range out.Output.Results {
		results[r.StepID] = r.Result
	}
	var sinks []Sink
	for _, step := range p.Steps {
		if depended[step.ID] {
			continue
		}
		r, ok := results[step.ID]
		if !ok && step.Result != nil {
			r, ok = *step.Result, true
		}
		if !ok || r.AttemptID == "" {
			return nil, fmt.Errorf("%w: sink %s has no completed attempt", ErrRecovery, step.ID)
		}
		if r.Artifact == "" || r.Artifact == p.Base {
			continue
		}
		sink := Sink{StepID: step.ID, Artifact: r.Artifact, LandingID: "plan-sink/" + p.ID + "/" + step.ID + "/" + r.AttemptID}
		if r.ExecutionToken != nil {
			sink.Source = &artifact.Source{Execution: r.ExecutionToken, AttemptID: r.AttemptID}
		}
		sinks = append(sinks, sink)
	}
	return sinks, nil
}

func (s *Supervisor) finishRun(ctx context.Context, rec RunRecord, p plan.Plan, out Outcome) (Outcome, error) {
	if rec.Phase == RunCompleted {
		if rec.Outcome != "success" {
			return out, fmt.Errorf("plan %s %s: %s", p.ID, rec.Outcome, rec.Error)
		}
		return out, nil
	}
	if rec.Phase != RunLanding {
		return out, fmt.Errorf("plan %s is %s", p.ID, rec.Phase)
	}
	proj, ok, err := s.deps.Artifacts.Project(ctx, rec.ProjectID)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("plan project vanished")
		}
		return out, err
	}
	if proj.Home != rec.Target {
		return out, fmt.Errorf("%w: plan %s target changed before landing", ErrRecovery, rec.PlanID)
	}
	for _, sink := range rec.Sinks {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var sources []artifact.Source
		if sink.Source != nil {
			sources = []artifact.Source{*sink.Source}
		}
		land, err := s.deps.Artifacts.LandOnce(ctx, sink.LandingID, proj, sink.Artifact, "plan "+p.ID, sources...)
		out.Landings = append(out.Landings, land)
		if err != nil {
			rec.Error = err.Error()
			saveErr := s.saveRun(ctx, &rec, RunLanding)
			return out, errors.Join(err, saveErr)
		}
	}
	if s.tasks != nil && rec.TaskID != "" {
		tracked, ok := s.tasks.Get(rec.TaskID)
		if !ok {
			return out, errors.New("plan task vanished")
		}
		if tracked.State != task.StateDone {
			if rec.Execution != nil {
				_, err = s.tasks.AdvanceExecution(*rec.Execution, task.StateDone)
			} else {
				_, err = s.tasks.Advance(rec.TaskID, task.StateDone)
			}
			if err != nil {
				return out, err
			}
		}
	}
	rec.Outcome, rec.Error = "success", ""
	if err := s.saveRun(ctx, &rec, RunCompleted); err != nil {
		return out, err
	}
	return out, nil
}

// Resume rebuilds scheduling from committed outputs. Every node consults its
// bound attempt before allocating a workspace, budget or model invocation.
func (r *Runs) Resume(ctx context.Context, p plan.Plan, deps Deps, runID string) (Outcome, error) {
	restored, err := restorePlan(ctx, p, deps)
	if err != nil {
		return Outcome{}, err
	}
	records, err := r.store.ListCheckpoints(ctx, workflow.CheckpointHistoryRequest{RunID: runID, Limit: 10000})
	if err != nil {
		return Outcome{}, err
	}
	if len(records) > 0 {
		latest := records[0]
		for _, rec := range records {
			if rec.Version > latest.Version {
				latest = rec
			}
		}
		if err := r.validateCheckpointCompletions(ctx, latest, restored); err != nil {
			return Outcome{}, err
		}
		switch latest.Status {
		case workflow.CheckpointRunning, workflow.CheckpointInterrupted, workflow.CheckpointCompleted:
			out, err := r.run(ctx, restored, deps, workflow.WithResume(workflow.ResumeRequest{RunID: runID, CheckpointID: latest.ID}))
			if err == nil {
				return out, nil
			}
			if !errors.Is(err, workflow.ErrCheckpointConflict) && !errors.Is(err, workflow.ErrCheckpointNotFound) {
				return out, err
			}
		case workflow.CheckpointFailed, workflow.CheckpointCanceled, workflow.CheckpointTerminated:
			// The old scheduler is terminal; only explicitly failed/expired
			// attempts with matching task authorization can enter retry.
		default:
			return Outcome{}, fmt.Errorf("%w: checkpoint state %s", ErrRecovery, latest.Status)
		}
	}
	return r.run(ctx, restored, deps, gopact.WithRunID(fmt.Sprintf("%s-recovery-%d", runID, time.Now().UnixNano())))
}

func (r *Runs) validateCheckpointCompletions(ctx context.Context, checkpoint workflow.CheckpointRecord, p plan.Plan) error {
	steps := map[string]plan.Step{}
	for _, step := range p.Steps {
		steps[step.ID] = step
	}
	if checkpoint.Status == workflow.CheckpointCompleted {
		for _, step := range p.Steps {
			if step.State != plan.StepDone || step.Result == nil {
				return fmt.Errorf("%w: completed checkpoint lacks bound step %s", ErrRecovery, step.ID)
			}
		}
	}
	var after int64
	for after < checkpoint.ConfirmedSequence {
		previous := after
		events, err := r.store.List(ctx, runlog.Query{RunID: checkpoint.RunID, After: after, Limit: 1000})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return fmt.Errorf("%w: checkpoint event history is incomplete", ErrRecovery)
		}
		for _, event := range events {
			if event.Sequence > checkpoint.ConfirmedSequence {
				break
			}
			after = event.Sequence
			if event.EventType == workflow.EventNodeCompleted {
				if step, ok := steps[event.NodeID]; ok && (step.State != plan.StepDone || step.Result == nil) {
					return fmt.Errorf("%w: checkpoint completed step %s without a bound result", ErrRecovery, event.NodeID)
				}
			}
		}
		if after == previous {
			return fmt.Errorf("%w: checkpoint event cursor cannot advance", ErrRecovery)
		}
	}
	return nil
}

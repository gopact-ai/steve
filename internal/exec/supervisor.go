package exec

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

// MaxRevisions bounds how many times a plan may be reshaped. Recovery
// (retry the same step elsewhere) is bounded separately; this is the limit on
// how often the planner gets to change its mind before the task is called
// off with an honest account of how far it got.
const MaxRevisions = 4

// PlanStore is the part of the plan store the supervisor drives: the current
// revision to reason about, and a place to put the next one.
type PlanStore interface {
	Recorder
	Latest(id string) (plan.Plan, bool)
	Revise(id string, steps []plan.Step, by, because string) (plan.Plan, error)
}

// Supervisor pairs a planner with the runtime that executes what it plans.
//
// The two halves stay separate on purpose. A planner decides *what* should
// happen and can be swapped — a declared workflow, a capability rule, a model
// that decomposes an open goal — while everything that must hold regardless
// of who planned it (placement against the live roster, the budget, the
// verification rule, the recovery and revision limits) lives in the runtime
// and is not negotiable by whatever produced the plan.
//
// The control loop is plan → execute → observe → {done | retry | revise}.
// Retry is the same plan with the failed step placed elsewhere; revise is a
// new revision from the planner, keeping every step that already finished.
// Both are ordinary edges, taken with the reason on record.
type Supervisor struct {
	planner          planner.Planner
	runs             *Runs
	deps             Deps
	plans            PlanStore
	fleet            *roster.Roster
	ledger           *ledger.Ledger
	owner            string
	tasks            *task.Store
	driverTTL        time.Duration
	recoveryMu       sync.Mutex
	recoveryPrepared bool
}

func NewSupervisor(p planner.Planner, deps Deps, store workflow.Store) *Supervisor {
	if store == nil {
		store = workflow.NewMemoryStore()
	}
	return &Supervisor{planner: p, runs: NewRuns(store), deps: deps, fleet: deps.Roster}
}

// SetPlans wires the plan store: step outcomes are written back as they
// happen, and revisions have somewhere to go.
func (s *Supervisor) SetExecution(r *execution.Registry) { s.deps.Executions = r }

func (s *Supervisor) SetPlans(store PlanStore) {
	s.plans = store
	s.deps.Recorder = store
}

// SetRecorder wires write-back alone, for callers with no revision store.
func (s *Supervisor) SetRecorder(r Recorder) { s.deps.Recorder = r }

func (s *Supervisor) Name() string { return s.planner.Name() }

// Runs exposes the runtime so callers can attach event sinks.
func (s *Supervisor) Runs() *Runs { return s.runs }

func (s *Supervisor) Plan(ctx context.Context, req planner.Request) (plan.Plan, error) {
	return s.planner.Plan(ctx, req)
}

// PrepareRulePlan freezes the pure rule result before task creation. Other
// planners use their independently admitted, retained native attempts.
func (s *Supervisor) PrepareRulePlan(ctx context.Context, goal, projectID string) (plan.Plan, bool, error) {
	switch s.planner.(type) {
	case planner.Rule, *planner.Rule:
		built, err := s.planner.Plan(ctx, planner.Request{Goal: goal, ProjectID: projectID})
		built.ProjectID = projectID
		return built, true, err
	default:
		return plan.Plan{}, false, nil
	}
}

func (s *Supervisor) ResumePlanning(ctx context.Context, attemptID string) (plan.Plan, error) {
	planner, ok := s.planner.(interface {
		ResumePlan(context.Context, string) (plan.Plan, error)
	})
	if !ok {
		return plan.Plan{}, fmt.Errorf("planner cannot resume retained planning")
	}
	return planner.ResumePlan(ctx, attemptID)
}

// Execute drives a plan to completion, revising it when execution finds the
// plan itself was wrong. It returns the last outcome either way: a plan that
// was called off is a result too, and the caller needs to see where it got.
func (s *Supervisor) Execute(ctx context.Context, p plan.Plan) (Outcome, error) {
	rec, err := s.opened(ctx, p)
	if err != nil {
		return Outcome{}, err
	}
	fresh := rec.fresh
	return s.runOwner(ctx, rec, func(ctx context.Context, rec RunRecord) (Outcome, error) {
		if rec.Phase == RunLanding || rec.Phase == RunCompleted {
			return s.finishRun(ctx, rec, p, Outcome{RunID: rec.RunID})
		}
		if err := s.prepareBase(ctx, &rec, &p); err != nil {
			return Outcome{}, err
		}
		s.reserve(ctx, p)
		defer s.unreserve(ctx, p)
		var out Outcome
		var err error
		if fresh {
			out, err = s.runs.Execute(ctx, p, s.deps)
		} else {
			out, err = s.runs.Resume(ctx, p, s.deps, rec.RunID)
		}
		if err != nil && ctx.Err() == nil {
			out, err = s.continueFrom(ctx, p, out, err)
		}
		return s.executed(ctx, rec, p, out, err)
	})
}

// continueFrom is the revise-and-rerun loop after a first run failed.
func (s *Supervisor) continueFrom(ctx context.Context, p plan.Plan, outcome Outcome, err error) (Outcome, error) {
	current := p
	for {
		if err == nil || ctx.Err() != nil {
			return outcome, err
		}
		var blocked *agentexec.RecoveryBlocked
		if errors.As(err, &blocked) {
			return outcome, err
		}
		if s.plans == nil || !NeedsRevision(err) || current.Fixed {
			return outcome, err
		}
		if current.Rev-1 >= MaxRevisions {
			return outcome, fmt.Errorf("called off after %d revisions: %w", current.Rev-1, err)
		}
		revised, revErr := s.revise(ctx, current, err)
		if revErr != nil {
			var retained *agentexec.RecoveryBlocked
			if errors.As(revErr, &retained) {
				return outcome, revErr
			}
			// The planner had nothing better. The original failure is the
			// one worth reporting; the planner's refusal is why it stands.
			log.Printf("exec: plan %s not revised: %v", current.ID, revErr)
			return outcome, err
		}
		current = revised
		rec, _, loadErr := s.loadRun(ctx, planRunID(current.ID))
		if loadErr != nil {
			return outcome, loadErr
		}
		current.Base = rec.Base
		rec.Rev, rec.RunID = current.Rev, runIDFor(current)
		if saveErr := s.saveRun(ctx, &rec, RunExecuting); saveErr != nil {
			return outcome, saveErr
		}
		outcome, err = s.runs.Execute(ctx, current, s.deps)
	}
}

// revise asks the planner for a new shape, given what actually happened,
// and stores it as the next revision with the trigger on record.
func (s *Supervisor) revise(ctx context.Context, current plan.Plan, cause error) (plan.Plan, error) {
	latest, ok := s.plans.Latest(current.ID)
	if !ok {
		return plan.Plan{}, fmt.Errorf("plan %s vanished", current.ID)
	}
	var candidates []roster.Candidate
	if s.fleet != nil {
		candidates = s.fleet.All(ctx)
	}
	request := planner.Request{
		Goal: latest.Goal, TaskID: latest.TaskID, ProjectID: latest.ProjectID, Current: latest,
		Trigger: cause.Error(), Roster: candidates,
	}
	var proposed plan.Plan
	var err error
	retainedID := ""
	if s.deps.Attempts != nil {
		records, loadErr := s.deps.Attempts.ForTask(ctx, latest.TaskID)
		if loadErr != nil {
			return plan.Plan{}, loadErr
		}
		prefix := fmt.Sprintf("plan/%s/r%d/prompt/", latest.TaskID, latest.Rev+1)
		var retained attempt.Record
		for _, record := range records {
			if record.Kind == attempt.KindPlan && strings.HasPrefix(record.TurnID, prefix) && strings.HasPrefix(record.Session, "ns_") && record.State != attempt.Superseded && (retained.ID == "" || record.StartedAt.After(retained.StartedAt)) {
				retained = record
			}
		}
		retainedID = retained.ID
	}
	if retainedID != "" {
		proposed, err = s.ResumePlanning(ctx, retainedID)
	} else {
		proposed, err = s.planner.Plan(ctx, request)
	}
	if err != nil {
		return plan.Plan{}, err
	}
	// Finished work stays finished. A planner may reshape what comes next;
	// it does not get to un-finish a step, and that is enforced here rather
	// than trusted to every planner — a revision that re-ran completed
	// steps would spend the budget twice and could hit the same finding
	// that triggered it.
	steps := keepFinished(latest, proposed.Steps)
	by, because := s.planner.Name(), cause.Error()
	if retainedID != "" {
		by, because = proposed.By, proposed.Because
	}
	revised, err := s.plans.Revise(current.ID, steps, by, because)
	if err != nil {
		return plan.Plan{}, err
	}
	log.Printf("exec: plan %s revised to rev %d by %s — %s", current.ID, revised.Rev, s.planner.Name(), cause)
	return revised, nil
}

// keepFinished carries each finished step's state and result from the
// current revision into the proposed one, matched by id.
func keepFinished(current plan.Plan, proposed []plan.Step) []plan.Step {
	done := map[string]plan.Step{}
	for _, s := range current.Steps {
		if s.State == plan.StepDone && s.Result != nil {
			done[s.ID] = s
		}
	}
	out := make([]plan.Step, len(proposed))
	for i, s := range proposed {
		if prior, ok := done[s.ID]; ok {
			s = prior
		} else if s.State == plan.StepDone {
			// A planner claiming a step is done that the record does not
			// show as done is not believed.
			s.State, s.Result = plan.StepPending, nil
		}
		out[i] = s
	}
	return out
}

// ReservationTTL bounds how long a plan's reserved capacity is held for a
// step that has not started yet.
var ReservationTTL = 45 * time.Minute

// reserve holds one endpoint slot per pending step on the machine
// placement would pick now, so parallel branches do not starve each other
// once they are running. A reservation that cannot be had is not an
// error: the step will wait for a slot when its turn comes.
func (s *Supervisor) reserve(ctx context.Context, p plan.Plan) {
	if s.deps.Attempts == nil || s.deps.Roster == nil || s.deps.Artifacts == nil {
		return
	}
	proj, ok, err := s.deps.Artifacts.Project(ctx, p.ProjectID)
	if err != nil || !ok {
		return
	}
	for _, step := range p.Steps {
		if step.State == plan.StepDone {
			continue
		}
		candidate, err := place(ctx, step, s.deps.Roster, proj)
		if err != nil || candidate.Slots <= 0 {
			continue
		}
		key := p.ID + "/" + step.ID
		if _, ok, _ := s.deps.Attempts.ReservationFor(ctx, key); ok {
			continue
		}
		if _, err := s.deps.Attempts.ReserveForIn(ctx, candidate.Region, key, candidate.Node, candidate.Harness, candidate.Slots, "plan "+p.ID, ReservationTTL); err != nil {
			log.Printf("exec: plan %s step %s: no capacity to reserve on %s: %v", p.ID, step.ID, endpointOf(candidate.Node, candidate.Harness), err)
		}
	}
}

func (s *Supervisor) unreserve(ctx context.Context, p plan.Plan) {
	if s.deps.Attempts == nil {
		return
	}
	for _, step := range p.Steps {
		s.deps.Attempts.ReleaseReservationFor(context.WithoutCancel(ctx), p.ID+"/"+step.ID)
	}
}

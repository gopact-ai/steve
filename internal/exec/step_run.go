package exec

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ctxpack"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/view"
)

// runStep is the node body: place, budget, assemble, run, verify. Everything
// here is a guarantee Steve makes regardless of what any agent decides, which
// is exactly why it lives on this side of the seam rather than in a prompt.
//
// Placement, the workspace and the attempt's identity are settled here. The
// attempt itself — its lease, its session's settlement, the terminal
// transition and cleanup — is lifecycle.Run's; the step's budget, context,
// checkpoints and verification are its hooks.
func runStep(ctx context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps) (plan.StepResult, error) {
	started := time.Now()
	failed := func(err error) (plan.StepResult, error) {
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	// Placement is solved here, immediately before the step runs, not when
	// the plan was written. A node that went away in between is the normal
	// case; on a retry this is what picks a different machine.
	if deps.Attempts == nil || deps.Artifacts == nil {
		return failed(fmt.Errorf("no attempts or artifacts wired: a step cannot be recorded"))
	}
	proj, ok, err := deps.Artifacts.Project(ctx, p.ProjectID)
	if err != nil || !ok {
		return failed(fmt.Errorf("plan %s: project %q is unknown", p.ID, p.ProjectID))
	}
	candidate, err := place(ctx, step, deps.Roster, proj)
	if err != nil {
		return failed(err)
	}
	// The step runs in its own worktree: from the plan's base, or from the
	// one step it continues, with what it merges laid out under inputs/.
	attemptID := attempt.NewID()
	var unresolved error
	if deps.Executions != nil {
		scope, err := deps.Executions.Begin(ctx, execution.Key{TaskID: p.TaskID, InstanceID: p.ID + "/" + step.ID, AttemptID: attemptID})
		if err != nil {
			return plan.StepResult{}, err
		}
		defer func() { scope.Finish(unresolved) }()
		ctx = scope.Context()
	}
	base, inputs := lineage(p, step, upstream)
	workspace, err := materialize(ctx, deps, p, candidate.Node, base, inputs, attemptID)
	if err != nil {
		return failed(err)
	}
	// A plan without a base starts from wherever the project was when the
	// first step materialised; that snapshot is the base from here on.
	if base == "" {
		base = workspace.Base
	}
	workID, err := stepHash(p, step, upstream)
	if err != nil {
		return plan.StepResult{}, err
	}
	spec := attempt.Spec{WorkID: workID, Execution: execution.Token(ctx),
		ID: attemptID, TaskID: p.TaskID, TurnID: p.ID + "/" + step.ID, Kind: attempt.KindStep, Project: p.ProjectID,
		Node: candidate.Node, Harness: candidate.Harness, Agent: candidate.Agent.ID, Slots: candidate.Slots,
		Region: candidate.Region, CanonicalRegion: deps.Roster.RegionOf(proj.Home.Node),
		Workspace: workspace, Scope: attempt.ScopePathSet, Touches: step.Touches, Base: base, By: "exec",
		Requires: step.Requires,
	}
	// Capacity reserved for this step ahead of time is taken over; a
	// reservation on another endpoint than placement chose is given back.
	if r, ok, _ := deps.Attempts.ReservationFor(ctx, spec.TurnID); ok {
		if r.Endpoint == endpointOf(candidate.Node, candidate.Harness) {
			spec.Reservation = r.ID
		} else {
			deps.Attempts.ReleaseReservationFor(ctx, spec.TurnID)
		}
	}
	// A retry is a takeover, not a fresh start: the previous attempt of
	// this step is superseded — its leases cut, its record pointing here —
	// so nothing it might still be doing can bind.
	previous, had, err := deps.Attempts.LatestForTurn(ctx, spec.TurnID)
	if err != nil {
		// The step never ran: its workspace is dropped when possible and
		// swept later otherwise; the placement error is what is reported.
		_ = deps.Artifacts.Discard(ctx, workspace)
		return failed(err)
	}
	supersede := ""
	if had && previous.TakeoverAllowed() {
		supersede = previous.ID
	}
	r := &stepRun{deps: deps, p: p, step: step, upstream: upstream, started: started, actor: "exec",
		candidate: candidate, workspace: workspace, inputs: inputs, at: harness.Placement{Node: candidate.Node, Harness: candidate.Harness}}
	var result plan.StepResult
	result, err, unresolved = r.run(ctx, spec, supersede)
	return result, err
}

// stepSettlement: a step is judged by its own verification, after what it
// made is published, and its completion is committed as prepared. Its
// session is the Runner's, closed when the prompt returned, so a hub
// session's unsettled end is a failure unless it is an unconfirmed stop —
// the step's own, or a verifier's whose exit nobody saw, either of which
// keeps the workspace. A node-owned session this process cannot vouch for
// — its prompt unsettled, its observer cancelled, a stage of its finish
// blocked — is the node's, quarantined until an observer comes back. One
// whose outcome is on record but whose close failed is not: the step's
// restore closes a failed step's session again (restoreStep).
var stepSettlement = lifecycle.Settlement{Quarantine: lifecycle.QuarantineManaged, DetachManaged: true, Detachment: lifecycle.DetachQuarantines, CancelDetaches: true, CommitAsGiven: true, QuarantineFinish: true, RetryCleanup: true}

// retainedStepRunner joins a step's node-owned session again.
type retainedStepRunner interface {
	AttachStep(context.Context, StepRequest, attempt.Record) (RetainedStep, error)
}

// retainedStepCloser closes a step's node-owned session by its record.
type retainedStepCloser interface {
	CloseRetainedStep(context.Context, attempt.Record) error
}

// stepRun is one plan step's side of the lifecycle: where it runs, what
// the agent is asked, what it made, and how the end is read.
type stepRun struct {
	deps     Deps
	p        plan.Plan
	step     plan.Step
	upstream []Result
	started  time.Time
	// actor signs the step's transitions: exec for a live step,
	// exec-recovery for one joined again.
	actor string
	// joined says the step's session was joined again: its answer comes
	// through the session, not the Runner.
	joined bool

	candidate roster.Candidate
	workspace project.Workspace
	inputs    []project.Input
	at        harness.Placement
	req       StepRequest
	// record is the attempt as last written or read; a node-owned session
	// the Runner records during the prompt updates it.
	record attempt.Record
	// managed says the record's session is the node's.
	managed bool
	// result is what the step made, filled by the prompt and amended by
	// the finish.
	result plan.StepResult
	// saved is a joined step's result already on the record, when its
	// prompt ended before this observer came back.
	saved *plan.StepResult
	// reserved says the budget was charged, and is the settle's to settle.
	reserved bool
	// promptErr is how the prompt ended, for the step's own error text.
	promptErr error
	// stage is the finish stage that could not be completed; refused says
	// the finish judged the work and failed it.
	stage   string
	refused bool
	// rejected is why the ledger refused the completion.
	rejected error
	// closed says the finish closed the node-owned session, so Run's own
	// close has nothing left to do.
	closed bool
	// waited is the full endpoint the open waited on.
	waited *attempt.NoSlot
}

// run takes a live step through Run.
func (r *stepRun) run(ctx context.Context, spec attempt.Spec, supersede string) (plan.StepResult, error, error) {
	o := r.options()
	o.Spec, o.Supersede, o.SlotPoll = spec, supersede, slotPollInterval
	o.Waiting = func(full attempt.NoSlot) {
		// Every slot is taken: wait for one rather than fail the step. The
		// plan's own deadline bounds the wait.
		r.waited = &full
		log.Printf("exec: step %s waits for a slot on %s", r.step.ID, full.Endpoint)
	}
	o.Lost = func() { log.Printf("exec: attempt %s lost its lease; cancelling step %s", spec.ID, r.step.ID) }
	// Placement was a decision on a snapshot; admission is the machine's
	// word on what it has now. A refusal fails this attempt and sends the
	// step back to placement, which will not pick the same agent first.
	o.Candidate, o.Requires, o.Uses, o.AdmitUnsure = r.candidate, r.step.Requires, r.candidate.Agent.MCPServers, true
	o.Workdir = r.workspace.Path
	o.Leased, o.Started, o.Wrap = r.reserve, r.start, r.wrap
	run, err := lifecycle.Run(ctx, o)
	return r.settle(run, err)
}

// resume joins a step's node-owned session again from the prompt on.
func (r *stepRun) resume(ctx context.Context, joined RetainedStep, replay *lifecycle.Outcome) (plan.StepResult, error, error) {
	o := r.options()
	o.Spec, o.Resume, o.Replay = r.record.Spec, true, replay
	o.Ask, o.AskUser, o.Observe = joined.Ask, joined.AskUser, joined.Observe
	run, err := lifecycle.Reattach(ctx, o, r.record, joined.Session)
	return r.settle(run, err)
}

func (r *stepRun) options() lifecycle.Options {
	o := lifecycle.Options{Attempts: r.deps.Attempts, Sessions: r, Workspaces: r.deps.Artifacts, Actor: r.actor, At: r.at,
		Ended: r.ended, Finish: r.finish, Settlement: stepSettlement}
	if r.deps.Roster != nil {
		o.Roster = r.deps.Roster
	}
	return o
}

// reserve charges the task's budget before the machine is asked: a spent
// budget is a refusal, not an interruption halfway through. The context
// the agent is given is assembled here too, before admission.
func (r *stepRun) reserve(ctx context.Context, e *lifecycle.Execution) (context.Context, error) {
	r.record = e.Record
	turnsLeft, deadline := 0, time.Time{}
	if r.deps.Budget != nil {
		var err error
		if turnsLeft, deadline, err = agentexec.ReserveBudget(r.deps.Budget, e.Record); err != nil {
			return ctx, ErrNoBudget{Cause: err}
		}
		r.reserved = true
	}
	refs, findings := inherited(r.upstream)
	payload, err := ctxpack.Build(ctxpack.Context{
		Goal:      r.step.Goal,
		Ancestry:  ancestry(r.p, r.step),
		Refs:      refs,
		Findings:  findings,
		Bearings:  ctxpack.Bearings(r.workspace.Path, refs) + inputsNote(r.inputs),
		Facts:     facts(r.candidate, r.deps.Roster.All(ctx)),
		TurnsLeft: turnsLeft,
		Deadline:  deadlineLabel(deadline),
	})
	if err != nil {
		return ctx, err
	}
	r.req = StepRequest{
		OpenFailure: r.openFailure,
		TaskID:      r.p.TaskID, PlanID: r.p.ID, StepID: r.step.ID,
		Agent: r.candidate.Agent.ID, Node: r.candidate.Node, Project: r.p.ProjectID, Workspace: r.workspace.Path,
		Goal: r.step.Goal, Context: payload,
	}
	return ctx, nil
}

// openFailure reads an open whose reply was lost: the node may hold a
// session for this attempt that this process cannot name.
func (r *stepRun) openFailure(cause error) error {
	if pending := agentexec.PendingNodeOpen(r.record, cause); pending != nil {
		return agentexec.Blocked(r.record, "open", "按原步骤标识请求打开节点会话", "原节点未返回完整的打开回执，会话可能已经创建。", "建议恢复原节点后核对打开记录，不重新提交原步骤。", pending)
	}
	return cause
}

// start keeps the running record and lets the Runner record the
// node-owned session it opens, before the prompt.
func (r *stepRun) start(ctx context.Context, e *lifecycle.Execution) error {
	r.record = e.Record
	r.req.RecordSession = func(id string) error {
		updated, err := r.deps.Attempts.RecordSession(ctx, r.record.ID, r.actor, id)
		if err == nil {
			r.record = updated
		}
		return err
	}
	return nil
}

// OpenSession hands Run the step's session: the Runner opens the real one
// when it prompts, with the servers the machine bound for the step.
func (r *stepRun) OpenSession(_ context.Context, _ harness.Placement, _, _ string, servers []acp.MCPServer) (harness.Runner, error) {
	r.req.MCP = servers
	return &stepSession{run: r}, nil
}

// CloseSession closes a node-owned session the finish did not close
// already. A hub session is the Runner's, closed when the prompt returned.
func (r *stepRun) CloseSession(ctx context.Context, _ harness.Placement, id string) error {
	if r.closed || !lifecycle.IsManaged(id) {
		return nil
	}
	closer, ok := r.deps.Runner.(retainedStepCloser)
	if !ok {
		return errors.New("retained step cleanup unavailable")
	}
	if err := closer.CloseRetainedStep(ctx, r.record); err != nil {
		return err
	}
	r.closed = true
	return nil
}

// stepSession is the step's session as Run drives it: the Runner opens the
// real one and prompts it with the assembled context. Its identity is the
// node-owned session the Runner records during the prompt, and nothing
// for a hub session the Runner closes on its way out.
type stepSession struct{ run *stepRun }

func (s *stepSession) ID() string { return s.run.record.Session }
func (s *stepSession) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	result, err := s.run.deps.Runner.RunStep(ctx, s.run.req)
	s.run.result = result
	return result.Answer, nil, err
}
func (*stepSession) Cancel(context.Context) error { return nil }
func (*stepSession) Abort()                       {}

// ended reads the prompt's end into the step's result: its identity, the
// session the Runner recorded, and what the turn cost.
func (r *stepRun) ended(e *lifecycle.Execution) {
	r.managed = lifecycle.IsManaged(r.record.Session)
	e.Record, e.Managed = r.record, r.managed
	r.promptErr = e.Outcome.Err
	switch {
	case r.saved != nil:
		r.result = *r.saved
	case r.joined:
		answer := e.Outcome.Answer
		r.result = plan.StepResult{Answer: answer, Refs: ParseRefs(answer), Findings: parseFindings(answer), Usage: spendOf(e.Outcome.Last)}
	}
	r.result.AttemptID, r.result.ExecutionToken = r.record.ID, r.record.Execution
	r.result.PlanRevision = r.p.Rev
	r.result.Agent, r.result.Node = r.record.Agent, r.record.Node
	r.result.StartedAt, r.result.EndedAt = r.started, time.Now()
	e.Usage = attemptUsage(r.result.Usage)
}

// finish is the step's completion: its session settled, what it made
// published and judged, its checkpoints written, and the completion that
// binds the result to the step's name — under the lease Run keeps.
func (r *stepRun) finish(ctx context.Context, e *lifecycle.Execution) (completion attempt.Completion, err error) {
	r.record, r.managed = e.Record, e.Managed
	defer func() { e.Record = r.record }()
	startingPhase := r.record.State
	if err := r.settleSession(ctx); err != nil {
		return attempt.Completion{}, err
	}
	published, err := r.publish(ctx)
	if err != nil {
		return attempt.Completion{}, err
	}
	if err := r.verify(ctx, published, startingPhase); err != nil {
		return attempt.Completion{}, err
	}
	return r.bind(ctx, published)
}

// settleSession closes a node-owned session still open and gives its
// endpoint back before anything else runs there: a verifier may need the
// very slot the step held.
func (r *stepRun) settleSession(ctx context.Context) error {
	if r.managed && r.record.State == attempt.Running {
		if closer, ok := r.deps.Runner.(retainedStepCloser); ok {
			if err := closer.CloseRetainedStep(ctx, r.record); err != nil {
				return r.blocked("close", err)
			}
			r.closed = true
		}
	}
	if err := r.deps.Attempts.ReleaseEndpointAfterSessionClosed(ctx, r.record.ID, "exec"); err != nil {
		return r.blocked("endpoint", err)
	}
	return nil
}

// publish makes what the step wrote an artifact before it is judged:
// verification runs against the same tree that will be bound, and the
// declared write scope is enforced on what was actually written.
func (r *stepRun) publish(ctx context.Context) (artifact.Manifest, error) {
	p, step, record := r.p, r.step, r.record
	published := artifact.Manifest{ID: r.result.Artifact}
	if stepPhase(record.State) < stepPhase(attempt.Snapshotted) || published.ID == "" {
		var err error
		published, _, err = r.deps.Artifacts.Publish(ctx, record.Workspace, record.Base, record.ID, "step "+step.ID+" of plan "+p.ID)
		if err != nil {
			return published, r.blocked("publish", err)
		}
		r.result.Artifact = published.ID
	}
	// A result that strayed outside its declared paths does not bind.
	if strayed, err := outsideScope(ctx, r.deps, p.ProjectID, record.Base, published.ID, step.Touches); err != nil {
		return published, r.blocked("scope", err)
	} else if len(strayed) > 0 {
		err := fmt.Errorf("step %s wrote outside its declared paths: %s", step.ID, strings.Join(strayed, ", "))
		return published, r.fail(err)
	}
	for _, to := range []attempt.State{attempt.Snapshotted, attempt.Published, attempt.Durable} {
		if err := r.advance(ctx, to); err != nil {
			return published, r.blocked("phase", err)
		}
	}
	return published, nil
}

// advance writes the next checkpoint. A node-owned session's step keeps
// its result and spend on the record at every phase, so an observer that
// comes back can finish from there without asking the node again.
func (r *stepRun) advance(ctx context.Context, to attempt.State) error {
	if stepPhase(r.record.State) >= stepPhase(to) {
		return nil
	}
	output, err := encodeStepOutput(r.p, r.step, r.upstream, r.result)
	if err != nil {
		return err
	}
	updated, err := r.deps.Attempts.Advance(ctx, r.record.ID, to, "exec", func(rec *attempt.Record) {
		if r.managed {
			rec.Result = &attempt.Result{Summary: clipSummary(r.result.Answer), Artifact: r.result.Artifact, Output: output}
			rec.Usage = attemptUsage(r.result.Usage)
		}
	})
	if err == nil {
		r.record = updated
	}
	return err
}

// verify is the step's own check. An agent reporting success is not
// evidence of success: a step reaches done only through its verification,
// and the verdict is written before it is acted on.
func (r *stepRun) verify(ctx context.Context, published artifact.Manifest, startingPhase attempt.State) error {
	step := r.step
	if step.Verify == nil || step.Verify.Kind == plan.VerifyNone {
		r.result.Verified = true
		return nil
	}
	if r.deps.Verifier == nil {
		return r.blocked("verifier", errors.New("step requires verification but no verifier is wired"))
	}
	if err := r.advance(ctx, attempt.Verifying); err != nil {
		return r.blocked("verify-phase", err)
	}
	passed, err := r.deps.Artifacts.Attested(ctx, published.ID, r.record.ID)
	if err != nil {
		return r.blocked("attestation", err)
	}
	if r.managed && step.Verify.Kind == plan.VerifyCommand && stepPhase(startingPhase) >= stepPhase(attempt.Verifying) && !passed {
		return r.blocked("verify-command", errors.New("原 shell 验证缺少可接续的调用回执，请先核对结果"))
	}
	var verifyErr error
	if !passed {
		if step.Verify.Kind == plan.VerifyCommand {
			if err := r.deps.Attempts.ArmSession(ctx, r.record.ID, "verify-command"); err != nil {
				return r.blocked("verify-marker", err)
			}
		}
		verifyErr = r.deps.Verifier.Verify(ctx, r.req, *step.Verify, r.result)
	}
	var auxiliary *agentexec.RecoveryBlocked
	if errors.As(verifyErr, &auxiliary) {
		return r.blocked("verify", verifyErr)
	}
	if step.Verify.Kind == plan.VerifyCommand {
		var exited interface{ ExitCode() int }
		if verifyErr == nil || (errors.As(verifyErr, &exited) && exited.ExitCode() >= 0) {
			if err := r.deps.Attempts.MarkSessionSettled(context.WithoutCancel(ctx), r.record.ID, "verify-command"); err != nil {
				return r.blocked("verify-marker", err)
			}
		} else if !errors.Is(verifyErr, harness.ErrStopUnconfirmed) {
			verifyErr = errors.Join(verifyErr, harness.ErrStopUnconfirmed)
		}
	}
	// Do not replace missing stop evidence with a cancelled attestation
	// write: that would release a directory a verifier may still use.
	if errors.Is(verifyErr, harness.ErrStopUnconfirmed) {
		return r.blocked("verify", verifyErr)
	}
	// The verdict is a fact either way, written before it is acted on.
	if !passed {
		if _, aerr := r.deps.Artifacts.Attest(ctx, attestationFor(r.p, step, r.record.ID, r.record.Node, published.ID, verifyErr)); aerr != nil {
			return r.blocked("attestation", aerr)
		}
	}
	if verifyErr != nil {
		return r.fail(errors.New("verification failed: " + verifyErr.Error()))
	}
	r.result.Verified = true
	// A verified step binds only on a durable, passing attestation from
	// this very attempt: the bind trusts the record, not the call stack.
	attested, err := r.deps.Artifacts.Attested(ctx, published.ID, r.record.ID)
	if err != nil || !attested {
		return r.blocked("attestation", errors.Join(err, fmt.Errorf("step %s: no durable passing attestation of %s on record", step.ID, published.ID)))
	}
	return nil
}

// bind prepares the completion: the result name at the version this
// attempt observed, the record at bind-ready, and the step's full output —
// answer, refs, findings, identity — committed with the Bound state. The
// result name, spend and terminal state commit under the same fences.
func (r *stepRun) bind(ctx context.Context, published artifact.Manifest) (attempt.Completion, error) {
	name := stepRef(r.p.TaskID, r.step.ID)
	current, _, err := r.deps.Artifacts.Resolve(ctx, name)
	if err != nil {
		return attempt.Completion{}, r.blocked("resolve", err)
	}
	if err := r.advance(ctx, attempt.BindReady); err != nil {
		return attempt.Completion{}, r.blocked("binding", err)
	}
	completion := attempt.Completion{
		Result: attempt.Result{Summary: clipSummary(r.result.Answer), Artifact: published.ID, Refs: refNames(r.result.Refs)},
		Usage:  attemptUsage(r.result.Usage), Binding: &attempt.NameBinding{Name: name, ExpectedVersion: current.Version},
	}
	r.result.Refs = append(r.result.Refs, plan.Ref{Kind: "artifact", Value: published.ID})
	if completion.Result.Output, err = encodeStepOutput(r.p, r.step, r.upstream, r.result); err != nil {
		return attempt.Completion{}, r.blocked("output", err)
	}
	return completion, nil
}

// fail is work the finish judged and refused: the attempt fails on it, on
// a node-owned session as on a hub one.
func (r *stepRun) fail(cause error) error {
	r.result.Error = cause.Error()
	r.refused = true
	return &lifecycle.Failure{Cause: cause}
}

// blocked is a finish stage that could not be completed. A node-owned
// session's step is left for the observer that comes back — unless what
// blocks it is another execution's own recovery, which is that one's to
// finish. A hub session's fails, or is quarantined when the stop it waits
// on is its own.
func (r *stepRun) blocked(stage string, cause error) error {
	r.stage = stage
	if r.managed {
		var auxiliary *agentexec.RecoveryBlocked
		if errors.As(cause, &auxiliary) && auxiliary.AttemptID != r.record.ID {
			return &lifecycle.Deferred{Cause: cause}
		}
		return cause
	}
	var owned interface{ UnsettledAttempt() string }
	if errors.Is(cause, harness.ErrStopUnconfirmed) && errors.As(cause, &owned) && owned.UnsettledAttempt() != r.record.ID {
		// Another execution's unconfirmed stop is not this attempt's.
		return &lifecycle.Failure{Cause: cause}
	}
	return cause
}

// wrap puts the step's words on a step of Run's own that failed.
func (r *stepRun) wrap(step lifecycle.Step, _ *lifecycle.Execution, err error) error {
	switch step {
	case lifecycle.StepOpen:
		// The wait's own end, not a ledger error that happens to wrap it.
		if r.waited != nil && (err == context.Canceled || err == context.DeadlineExceeded) {
			return fmt.Errorf("%w while waiting: %v", err, *r.waited)
		}
	case lifecycle.StepAdmit:
		var refused *lifecycle.Refused
		if errors.As(err, &refused) {
			return ErrNowhereToRun{StepID: r.step.ID, Requires: r.step.Requires,
				Reasons: nodewire.Place(r.candidate.Node) + " refused at admission: " + refused.Admission.Unmet()}
		}
	case lifecycle.StepFinish:
		// The ledger refused the completion: its workspace is retained, and
		// retrying the agent would hide the original completion conflict.
		r.rejected = err
		return fmt.Errorf("%w: %w", ErrCompletion, err)
	}
	return err
}

// settle reads how the run ended into the step's result and error, and
// what stays unresolved for the execution scope. The budget is settled on
// a terminal record whose writer is known to have stopped.
func (r *stepRun) settle(run lifecycle.Result, err error) (plan.StepResult, error, error) {
	if run.Record.ID != "" {
		r.record = run.Record
	}
	var step *lifecycle.StepError
	errors.As(err, &step)
	var detached *execution.RetainedObserverDetached
	var deferred *lifecycle.Deferred
	var unresolved error
	// prompted says the error is the prompt's own — how it ended, or the
	// cancellation on its heels — and not a step of Run's, the finish's
	// refusal or a stage of it that could not be completed.
	prompted := false
	switch {
	case !run.Driven:
		// The step never ran: what stopped it is the result.
		cause := err
		if step != nil {
			cause = step.Err
		}
		r.result, err = plan.StepResult{StartedAt: r.started, EndedAt: time.Now(), Error: cause.Error()}, cause
	case errors.As(err, &deferred):
		return r.result, deferred.Cause, nil
	case errors.As(err, &detached):
		unresolved = detached
		err = r.detachment(step, detached)
	case run.Unsettled:
		// A stop nobody confirmed: the prompt's own, or a verifier's whose
		// exit was not seen.
		unresolved = err
		if errors.Is(r.promptErr, harness.ErrStopUnconfirmed) {
			r.result.Error = err.Error()
			err = agentexec.Blocked(r.record, "failure", "保存原步骤的失败结果", "原命令已经返回，但结果尚未持久保存。", "建议恢复存储后检查同一次执行。", err)
		}
	case err != nil:
		switch {
		case step != nil && step.Step == lifecycle.StepFinish && r.rejected != nil:
			r.result.Error = "complete result: " + r.rejected.Error()
			err = step.Err
		case r.stage != "":
			// A hub session's finish stage that could not be completed is
			// the attempt's failure, not the step result's, as before.
		default:
			prompted = step == nil && !r.refused
			r.result.Error = err.Error()
		}
	}
	if r.reserved && r.record.State.Terminal() && !r.record.Unsettled {
		if budgetErr := agentexec.SettleBudget(r.deps.Budget, r.record, err); budgetErr != nil {
			if err == nil {
				return r.result, agentexec.Blocked(r.record, "accounting", "保存步骤的用量与预算", "步骤结果已经提交，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", budgetErr), unresolved
			}
			unresolved = agentexec.Blocked(r.record, "accounting", "保存原步骤的用量与预算", "失败结果已保存，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", budgetErr)
			return r.result, r.unresolvedFailure(err, prompted, &unresolved), unresolved
		}
	}
	if run.Managed && run.CleanupErr != nil {
		// The outcome is on record and its budget settled, but the
		// node-owned session's close did not confirm its process exited:
		// the session, the bindings and the workspace stay with the node.
		if err == nil {
			// The result is committed, and delivered; what the node keeps is
			// the node's until it is reachable again.
			log.Printf("exec: step %s: attempt %s is bound, but its node session was not released: %v", r.step.ID, r.record.ID, run.CleanupErr)
			return r.result, nil, nil
		}
		// A failed step's session is closed again when the step is restored.
		unresolved = agentexec.Blocked(r.record, "cleanup", "释放已结束步骤的原会话", "失败结果已保存，但原会话或工作区尚未释放。", "建议恢复原节点后重新检查。", run.CleanupErr)
		return r.result, r.unresolvedFailure(err, prompted, &unresolved), unresolved
	}
	return r.result, err, unresolved
}

// unresolvedFailure is a failed step whose settlement — its budget, its
// session — is unresolved, as the step reported it before: a prompt that
// failed carries the block as a failure question, in the words of the
// path that asked (live, or joined again, where it is a detached
// observer's); a finish that refused the work, or a step that failed
// before its prompt, returns its cause and leaves the block to the
// execution scope, where the retry finds it.
func (r *stepRun) unresolvedFailure(cause error, prompted bool, unresolved *error) error {
	if !prompted {
		return cause
	}
	if r.joined {
		*unresolved = retainedStepDetached(r.record, *unresolved)
		return agentexec.Blocked(r.record, "failure", "连接原步骤的节点并核对已接受命令", "原步骤暂时不能安全接续。", "建议恢复原节点或存储，再检查同一次执行。", *unresolved)
	}
	return agentexec.Blocked(r.record, "failure", "保存原步骤的失败结果", "原命令已经返回，但结果尚未持久保存。", "建议恢复存储后检查同一次执行。", *unresolved)
}

// detachment is a detached observer in the step's words: which stage it
// left, and how the observer that comes back should read it.
func (r *stepRun) detachment(step *lifecycle.StepError, detached *execution.RetainedObserverDetached) error {
	// origin is the step of Run the observer left at; at is the stage it
	// reads as: a failure whose own transition could not be written is
	// not a finish stage, whatever step it surfaced at.
	origin := lifecycle.StepDrive
	if step != nil {
		origin = step.Step
	}
	at := origin
	if at == lifecycle.StepFinish && (r.promptErr != nil || r.refused) {
		at = lifecycle.StepDrive
	}
	switch {
	case at == lifecycle.StepFinish:
		stage := r.stage
		if stage == "" {
			stage = "completion"
		}
		return agentexec.Blocked(r.record, stage, "保存原步骤的产物、验证与结果", "原命令已返回，但步骤还没有完整提交。", "建议恢复原节点或存储，继续核对这次执行。", detached)
	case r.joined:
		// A joined observer that lost the prompt — unsettled, cancelled —
		// is an observer's loss; only a settled end whose marker or failure
		// could not be written is the ledger's.
		code := "observer"
		switch origin {
		case lifecycle.StepSettle:
			code = "marker"
		case lifecycle.StepFinish:
			code = "failure"
			if r.promptErr != nil && !r.refused {
				r.result.Error = r.promptErr.Error()
			}
		}
		return agentexec.Blocked(r.record, code, "连接原步骤的节点并核对已接受命令", "原步骤暂时不能安全接续。", "建议恢复原节点或存储，再检查同一次执行。", detached)
	case at == lifecycle.StepSettle:
		return agentexec.Blocked(r.record, "marker", "保存原步骤的结束回执", "原命令结果已经返回，但结束回执尚未保存。", "建议恢复存储后重新核对原步骤。", detached)
	default:
		if r.promptErr != nil && !r.refused {
			r.result.Error = r.promptErr.Error()
		}
		return agentexec.Blocked(r.record, "failure", "保存原步骤的失败结果", "原命令已经返回，但结果尚未持久保存。", "建议恢复存储后检查同一次执行。", detached)
	}
}

// spendOf is a step's spend as the harness last reported it.
func spendOf(last view.Progress) *plan.Usage {
	u := last.Usage
	return &plan.Usage{Model: last.Settings.Model, Input: int64(u.InputTokens), Output: int64(u.OutputTokens), CachedRead: int64(u.CacheReadTokens), CachedWrite: int64(u.CacheWriteTokens), Context: int64(u.ContextTokens), Reported: u.TokensReported()}
}

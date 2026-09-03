// Package exec runs a plan by compiling it into a gopact workflow.
//
// The division is deliberate. A plan (internal/plan) is the *description*:
// persisted, versioned, validated, and the thing a person argues with. The
// workflow is the *runtime*: gopact owns scheduling, joins, checkpoints,
// recovery, the run log and same-run control, and it does all of that better
// than a scheduler written here would.
//
// What stays Steve's own is everything gopact has no opinion about, because
// its nodes are in-process Go functions: which machine a step runs on
// (internal/roster), how that machine is reached (internal/node), and what
// the agent over there is allowed to see (internal/ctxpack). Those three are
// the node body; gopact runs the graph around them.
package exec

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ctxpack"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

// maxParallelSteps bounds how many steps of one plan run at once. A step
// spends nearly all its time waiting on an agent on another machine, so the
// ceiling is about not flooding the nodes, not about local CPU.
const maxParallelSteps = 8

// StepRequest is one step handed to whatever actually drives agents.
type StepRequest struct {
	// MCP are the servers the machine bound for this step's session, as
	// launchers, on top of what the agent's capabilities assemble.
	MCP    []acp.MCPServer
	TaskID string
	PlanID string
	StepID string
	Agent  string
	Node   string
	// Project is the plan's project; Workspace is the directory on Node the
	// step runs in, materialised for it before the runner is called.
	Project   string
	Workspace string
	Goal      string
	Context   ctxpack.Context
}

// Runner performs one step's work. The gateway satisfies it by opening a
// session on the placed agent and prompting it.
type Runner interface {
	RunStep(ctx context.Context, req StepRequest) (plan.StepResult, error)
}

// Verifier checks a finished step. It is separate from Runner because the
// entire point is not to take the agent's word for it.
type Verifier interface {
	Verify(ctx context.Context, req StepRequest, v plan.Verify, result plan.StepResult) error
}

// Budget is checked at admission — before a step starts — so a spent budget
// is a refusal rather than an interruption halfway through.
type Budget interface {
	Reserve(taskID string) (turnsLeft int, deadline time.Time, err error)
}

// Recorder writes a step's outcome back to the plan. Without it the plan
// store still says "pending" after the work is done, and every surface that
// reads it — the card, /plans, the dashboard, the TUI — reports an empty
// plan while the run log says otherwise.
type Recorder interface {
	RecordStep(planID string, step plan.Step) error
}

// Deps are what a compiled plan needs to run.
type Deps struct {
	Roster   *roster.Roster
	Runner   Runner
	Verifier Verifier
	Budget   Budget
	Recorder Recorder
	// Workspaces answers where a step runs, given its project and the node
	// placement chose. It is asked after placement and before the budget
	// is touched: a step that cannot get a directory has not started.
	Workspaces project.Workspaces
	// Attempts leases and records each step's execution; Artifacts
	// publishes what it made and binds the step's name to it. Both are
	// required: a step that cannot be recorded does not run.
	Attempts  *attempt.Service
	Artifacts *artifact.Store
	// recoveries counts retries across the run; Runs.Execute sets it.
	recoveries *atomic.Int64
}

// MaxRecoveries is how many more times a step is tried after its first
// attempt fails — on an agent that has not failed it yet when one exists,
// otherwise the same one again. It is per step, and it is bounded here
// rather than by the budget alone, because a step that keeps failing should
// reach the planner as "this keeps failing" while there is budget left to
// do something about it.
const MaxRecoveries = 4

// ErrExhausted is a step Steve has given up on: every recovery spent. It is
// the moment the run is allowed to fail, and what the planner is asked to
// revise around.
type ErrExhausted struct {
	StepID   string
	Attempts int
	Cause    error
}

func (e ErrExhausted) Error() string {
	return fmt.Sprintf("step %q failed %d times; last: %v", e.StepID, e.Attempts, e.Cause)
}

func (e ErrExhausted) Unwrap() error { return e.Cause }

// ErrNoBudget is an admission refusal. It is not retried: trying again does
// not create budget.
type ErrNoBudget struct{ Cause error }

func (e ErrNoBudget) Error() string { return e.Cause.Error() }
func (e ErrNoBudget) Unwrap() error { return e.Cause }

// Result is one step's outcome as it flows between workflow nodes.
type Result struct {
	StepID string
	Result plan.StepResult
}

// Output is what a finished plan returns: every terminal step's result.
type Output struct {
	Results []Result
}

// ErrInvalidated is a step succeeding and finding out that steps after it
// are wrong. It is not a failure of the step; it is the plan asking to be
// revised, which is why recovery treats it differently from an error.
type ErrInvalidated struct {
	StepID   string
	Findings []plan.Finding
}

func (e ErrInvalidated) Error() string {
	texts := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		texts = append(texts, f.Text)
	}
	return fmt.Sprintf("step %q found that later steps are wrong: %s", e.StepID, strings.Join(texts, "; "))
}

func invalidated(result plan.StepResult) error {
	var hits []plan.Finding
	for _, f := range result.Findings {
		if len(f.Invalidates) > 0 {
			hits = append(hits, f)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return ErrInvalidated{Findings: hits}
}

// ErrNowhereToRun is a placement that cannot be satisfied. It carries the
// roster's reasons, because "no agent available" sends someone reading config
// files when the real answer is usually "a node is down".
type ErrNowhereToRun struct {
	StepID   string
	Requires []string
	Reasons  string
}

func (e ErrNowhereToRun) Error() string {
	return fmt.Sprintf("step %q needs %v but nothing can run it: %s", e.StepID, e.Requires, e.Reasons)
}

// Compile turns a plan into a runnable workflow.
//
// Every step becomes a Merge node regardless of its in-degree: gopact refuses
// an implicit join, which is the same rule the plan already states, and one
// node shape keeps the compiler honest about fan-in instead of special-casing
// it.
func Compile(p plan.Plan, deps Deps, store workflow.Store) (*workflow.Workflow[string, Output], error) {
	if err := plan.Validate(p); err != nil {
		return nil, err
	}
	// gopact defaults to maxParallelism 1, so a fan-out would run serially
	// unless this is set. Independent steps in a plan are independent by
	// construction — they were placed on different machines precisely so
	// they could overlap.
	options := []workflow.BuildOption{workflow.WithMaxParallelism(maxParallelSteps)}
	if store != nil {
		options = append(options, workflow.WithStore(store))
	}
	wf := workflow.New[string, Output]("plan-"+p.ID, options...)

	nodes := make(map[string]*workflow.Node[workflow.Inputs, Result], len(p.Steps))
	byID := make(map[string]plan.Step, len(p.Steps))
	for _, s := range p.Steps {
		byID[s.ID] = s
	}

	// start exists so a step with no dependencies still has an inbound edge:
	// gopact needs one entry, and a plan may begin with several steps at once.
	start := wf.Node("start", func(_ context.Context, goal string) (Result, error) {
		return Result{StepID: "start"}, nil
	})
	wf.Entry(start)

	for _, s := range p.Steps {
		step := s
		deps := deps
		upstreamIDs := dependencies(step)
		nodes[step.ID] = wf.Merge(step.ID, func(ctx context.Context, in workflow.Inputs) (Result, error) {
			upstream := make([]Result, 0, len(upstreamIDs))
			for _, id := range upstreamIDs {
				value, err := in.One(nodes[id])
				if err != nil {
					return Result{}, err
				}
				upstream = append(upstream, value)
			}
			// A step that already finished in an earlier revision is not
			// run again: the model may reshape what comes after it, but
			// finished work is finished, and its result is on record.
			if step.State == plan.StepDone && step.Result != nil {
				return Result{StepID: step.ID, Result: *step.Result}, nil
			}
			// Recovery happens here, inside the node, and not by failing
			// the run and retrying it. The runtime's failure semantics are
			// fail-fast — one node failing cancels the others — which is
			// right for a workflow whose nodes depend on each other and
			// wrong for a plan whose branches were placed on different
			// machines precisely because they do not. A branch that is
			// retrying must not take a healthy branch down with it.
			out, err := runStepWithRecovery(ctx, p, step, upstream, deps)
			if err == nil {
				if inv, ok := invalidated(out).(ErrInvalidated); ok {
					// The step succeeded and learned something that makes
					// later steps wrong. Stop here, on purpose: running
					// them anyway would spend budget on work the plan
					// already knows is mistaken. This is the replan edge.
					inv.StepID = step.ID
					return Result{StepID: step.ID, Result: out}, inv
				}
			}
			return Result{StepID: step.ID, Result: out}, err
		})
	}

	depended := map[string]bool{}
	for _, s := range p.Steps {
		for _, id := range dependencies(s) {
			depended[id] = true
		}
	}
	for _, s := range p.Steps {
		upstream := dependencies(s)
		if len(upstream) == 0 {
			wf.Edge(start, nodes[s.ID])
			continue
		}
		for _, id := range upstream {
			wf.Edge(nodes[id], nodes[s.ID])
		}
	}

	terminal := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		if !depended[s.ID] {
			terminal = append(terminal, s.ID)
		}
	}
	sort.Strings(terminal)

	collect := wf.Merge("collect", func(_ context.Context, in workflow.Inputs) (Output, error) {
		out := Output{}
		for _, id := range terminal {
			value, err := in.One(nodes[id])
			if err != nil {
				return Output{}, err
			}
			out.Results = append(out.Results, value)
		}
		return out, nil
	})
	for _, id := range terminal {
		wf.Edge(nodes[id], collect)
	}
	wf.Exit(collect)
	return wf, nil
}

// record writes what happened to a step back to the plan. A failure to record
// is logged rather than raised: losing the bookkeeping must not also lose the
// work, and the run log still holds the truth.
func record(deps Deps, planID string, step plan.Step, result plan.StepResult, runErr error) {
	if deps.Recorder == nil {
		return
	}
	step.Result = &result
	step.State = plan.StepDone
	if runErr != nil {
		step.State = plan.StepFailed
	}
	if err := deps.Recorder.RecordStep(planID, step); err != nil {
		log.Printf("exec: record step %s of plan %s: %v", step.ID, planID, err)
	}
}

// runStepWithRecovery tries a step until it succeeds or Steve gives up.
// Each attempt is recorded as it happens, so the plan store — and every
// screen that reads it — shows a step mid-retry rather than pending.
func runStepWithRecovery(ctx context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps) (plan.StepResult, error) {
	var last plan.StepResult
	var lastErr error
	for attempt := 0; attempt <= MaxRecoveries; attempt++ {
		step.Attempts++
		out, err := runStep(ctx, p, step, upstream, deps)
		record(deps, p.ID, step, out, err)
		if err == nil {
			return out, nil
		}
		last, lastErr = out, err
		// Some failures do not get better by trying again.
		if ctx.Err() != nil {
			return out, err
		}
		var nowhere ErrNowhereToRun
		var noBudget ErrNoBudget
		if errors.As(err, &nowhere) || errors.As(err, &noBudget) {
			return out, err
		}
		if out.Agent != "" && !contains(step.Tried, out.Agent) {
			step.Tried = append(step.Tried, out.Agent)
		}
		if attempt < MaxRecoveries {
			if deps.recoveries != nil {
				deps.recoveries.Add(1)
			}
			log.Printf("exec: step %q attempt %d on %s failed: %v — retrying", step.ID, step.Attempts, out.Agent, err)
		}
	}
	return last, ErrExhausted{StepID: step.ID, Attempts: step.Attempts, Cause: lastErr}
}

// slotPollInterval is how often a step waiting for capacity looks again.
var slotPollInterval = 3 * time.Second

func endpointOf(node, harness string) string {
	if node == "" {
		node = "hub"
	}
	return "endpoint:" + node + "/" + harness
}

// admits says why a candidate may not hold the project's data, or "".
func admits(p project.Project, c roster.Candidate) string {
	if p.Level == project.LevelSealed && c.Node != p.Home.Node {
		return "sealed project " + p.ID + " runs only at " + placeLabel(p.Home.Node)
	}
	if !p.Level.OrDefault().Admits(c.Level.OrDefault()) {
		return placeLabel(c.Node) + " is " + string(c.Level.OrDefault()) + ", project " + p.ID + " is " + string(p.Level.OrDefault())
	}
	return ""
}

func admitted(p project.Project, in []roster.Candidate) []roster.Candidate {
	out := in[:0:0]
	for _, c := range in {
		if admits(p, c) == "" {
			out = append(out, c)
		}
	}
	return out
}

func placeLabel(node string) string { return nodewire.Place(node) }

// materialize asks for the step's own worktree on the node placement chose.
func materialize(ctx context.Context, deps Deps, p plan.Plan, node, base string, inputs []project.Input, owner string) (project.Workspace, error) {
	if deps.Workspaces == nil {
		return project.Workspace{}, fmt.Errorf("no workspaces wired: a step has nowhere to run")
	}
	ws, err := deps.Workspaces.Materialize(ctx, project.Request{Project: p.ProjectID, Node: node, Isolated: true, Base: base, Inputs: inputs, Owner: owner})
	if err != nil {
		return project.Workspace{}, fmt.Errorf("workspace for step: %w", err)
	}
	return ws, nil
}

// lineage decides what a step starts from. One dependency and nothing to
// merge: continue that step's result. Otherwise: the plan's base, with
// every dependency's result under inputs/<step>.
func lineage(p plan.Plan, step plan.Step, upstream []Result) (string, []project.Input) {
	artifacts := map[string]string{}
	for _, r := range upstream {
		if r.Result.Artifact != "" {
			artifacts[r.StepID] = r.Result.Artifact
		}
	}
	if len(step.Merge) == 0 && len(step.Needs) == 1 {
		if a := artifacts[step.Needs[0]]; a != "" {
			return a, nil
		}
	}
	var inputs []project.Input
	for _, id := range dependencies(step) {
		if a := artifacts[id]; a != "" {
			inputs = append(inputs, project.Input{Name: id, Artifact: a})
		}
	}
	return p.Base, inputs
}

// inputsNote tells the agent what inputs/ is, in the bearings.
func inputsNote(inputs []project.Input) string {
	if len(inputs) == 0 {
		return ""
	}
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		names = append(names, "inputs/"+in.Name+"/")
	}
	return "\n\nEarlier steps' results are laid out read-only at " + strings.Join(names, ", ") +
		". Converge what they made into the working tree itself; inputs/ is dropped when you are done and is not part of your result."
}

// outsideScope lists the changed paths that no declared prefix covers. An
// empty declaration means the whole worktree.
func outsideScope(ctx context.Context, deps Deps, projectID, base, artifactID string, touches []string) ([]string, error) {
	if len(touches) == 0 || base == "" || artifactID == base {
		return nil, nil
	}
	changed, err := deps.Artifacts.Changed(ctx, projectID, base, artifactID)
	if err != nil {
		return nil, fmt.Errorf("compare result with base: %w", err)
	}
	var strayed []string
	for _, path := range changed {
		if !plan.Covers(touches, path) {
			strayed = append(strayed, path)
		}
	}
	return strayed, nil
}

// attestationFor turns a verification into the fact it records.
func attestationFor(p plan.Plan, step plan.Step, attemptID, node, artifactID string, verifyErr error) artifact.Attestation {
	a := artifact.Attestation{Artifact: artifactID, Project: p.ProjectID, TaskID: p.TaskID, Step: step.ID, Node: node, By: attemptID, Verdict: "pass"}
	switch step.Verify.Kind {
	case plan.VerifyCommand:
		a.Kind, a.Verifier = "command", step.Verify.Command
	case plan.VerifyAgent:
		a.Kind, a.Verifier = "agent", step.Verify.Agent
	}
	if verifyErr != nil {
		a.Verdict, a.Detail = "fail", verifyErr.Error()
	}
	return a
}

// stepRef is the name a step's result is bound to.
func stepRef(taskID, stepID string) string { return "steve/" + taskID + "/" + stepID }

func refNames(refs []plan.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Kind+":"+r.Value)
	}
	return out
}

func clipSummary(text string) string {
	runes := []rune(text)
	if len(runes) <= 200 {
		return text
	}
	return string(runes[:200]) + "…"
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// dependencies is a step's inbound edges. Needs and Merge are the same thing
// to the graph; they differ in what the plan is claiming, not in how the
// scheduler treats them.
func dependencies(s plan.Step) []string {
	out := make([]string, 0, len(s.Needs)+len(s.Merge))
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, s.Needs...), s.Merge...) {
		if seen[id] {
			continue // Validate refuses this; belt and braces for the graph
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// runStep is the node body: place, budget, assemble, run, verify. Everything
// here is a guarantee Steve makes regardless of what any agent decides, which
// is exactly why it lives on this side of the seam rather than in a prompt.
func runStep(ctx context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps) (plan.StepResult, error) {
	started := time.Now()

	// Placement is solved here, immediately before the step runs, not when
	// the plan was written. A node that went away in between is the normal
	// case; on a retry this is what picks a different machine.
	if deps.Attempts == nil || deps.Artifacts == nil {
		err := fmt.Errorf("no attempts or artifacts wired: a step cannot be recorded")
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	proj, ok, err := deps.Artifacts.Project(ctx, p.ProjectID)
	if err != nil || !ok {
		err := fmt.Errorf("plan %s: project %q is unknown", p.ID, p.ProjectID)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	candidate, err := place(ctx, step, deps.Roster, proj)
	if err != nil {
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	// The step runs in its own worktree: from the plan's base, or from the
	// one step it continues, with what it merges laid out under inputs/.
	attemptID := attempt.NewID()
	base, inputs := lineage(p, step, upstream)
	workspace, err := materialize(ctx, deps, p, candidate.Node, base, inputs, attemptID)
	if err != nil {
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	// A plan without a base starts from wherever the project was when the
	// first step materialised; that snapshot is the base from here on.
	if base == "" {
		base = workspace.Base
	}
	spec := attempt.Spec{
		ID: attemptID, TaskID: p.TaskID, TurnID: p.ID + "/" + step.ID, Kind: attempt.KindStep, Project: p.ProjectID,
		Node: candidate.Node, Harness: candidate.Harness, Agent: candidate.Agent.ID, Slots: candidate.Slots,
		Region: candidate.Region, CanonicalRegion: deps.Roster.RegionOf(proj.Home.Node),
		Workspace: workspace, Scope: attempt.ScopePathSet, Touches: step.Touches, Base: base, By: "exec",
		Requires: step.Requires,
	}
	// A retry is a takeover, not a fresh start: the previous attempt of
	// this step is superseded — its leases cut, its record pointing here —
	// so nothing it might still be doing can bind.
	// Capacity reserved for this step ahead of time is taken over; a
	// reservation on another endpoint than placement chose is given back.
	if r, ok, _ := deps.Attempts.ReservationFor(ctx, spec.TurnID); ok {
		if r.Endpoint == endpointOf(candidate.Node, candidate.Harness) {
			spec.Reservation = r.ID
		} else {
			deps.Attempts.ReleaseReservationFor(ctx, spec.TurnID)
		}
	}
	var record attempt.Record
	previous, had, err := deps.Attempts.LatestForTurn(ctx, spec.TurnID)
	takeover := err == nil && had && previous.TakeoverAllowed()
	for err == nil {
		if takeover {
			record, err = deps.Attempts.Supersede(ctx, previous.ID, spec, "exec")
		} else {
			record, err = deps.Attempts.Open(ctx, spec)
		}
		var full attempt.NoSlot
		if err == nil || !errors.As(err, &full) {
			break
		}
		// Every slot is taken: wait for one rather than fail the step. The
		// plan's own deadline bounds the wait.
		log.Printf("exec: step %s waits for a slot on %s", step.ID, full.Endpoint)
		select {
		case <-ctx.Done():
			err = fmt.Errorf("%w while waiting: %v", ctx.Err(), full)
		case <-time.After(slotPollInterval):
			err = nil
		}
	}
	if err != nil {
		_ = deps.Artifacts.Discard(ctx, workspace)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	stepCtx, stop := context.WithCancel(ctx)
	defer stop()
	lost := deps.Attempts.Heartbeat(stepCtx, record.ID)
	go func() {
		select {
		case <-lost:
			log.Printf("exec: attempt %s lost its lease; cancelling step %s", record.ID, step.ID)
			stop()
		case <-stepCtx.Done():
		}
	}()
	ctx = stepCtx
	fail := func(cause error) {
		if _, ferr := deps.Attempts.Fail(context.WithoutCancel(ctx), record.ID, "exec", cause.Error()); ferr != nil {
			log.Printf("exec: attempt %s could not be failed: %v", record.ID, ferr)
		}
		_ = deps.Artifacts.Discard(context.WithoutCancel(ctx), workspace)
	}

	turnsLeft, deadline := 0, time.Time{}
	if deps.Budget != nil {
		turnsLeft, deadline, err = deps.Budget.Reserve(p.TaskID)
		if err != nil {
			return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, ErrNoBudget{Cause: err}
		}
	}

	refs, findings := inherited(upstream)
	payload, err := ctxpack.Build(ctxpack.Context{
		Goal:      step.Goal,
		Ancestry:  ancestry(p, step),
		Refs:      refs,
		Findings:  findings,
		Bearings:  ctxpack.Bearings(workspace.Path, refs) + inputsNote(inputs),
		Facts:     facts(candidate, deps.Roster.All(ctx)),
		TurnsLeft: turnsLeft,
		Deadline:  deadlineLabel(deadline),
	})
	if err != nil {
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}

	req := StepRequest{
		TaskID: p.TaskID, PlanID: p.ID, StepID: step.ID,
		Agent: candidate.Agent.ID, Node: candidate.Node, Project: p.ProjectID, Workspace: workspace.Path,
		Goal: step.Goal, Context: payload,
	}
	// Placement was a decision on a snapshot; admission is the machine's
	// word on what it has now. A refusal fails this attempt and sends the
	// step back to placement, which will not pick the same agent first.
	admission, bindings, err := deps.Roster.Admit(ctx, candidate, step.Requires, candidate.Agent.MCPServers, record.ID)
	if err != nil {
		fail(err)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	if admission.Refused() {
		err := ErrNowhereToRun{StepID: step.ID, Requires: step.Requires,
			Reasons: nodewire.Place(candidate.Node) + " refused at admission: " + admission.Unmet()}
		fail(err)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	req.MCP = roster.ToMCP(bindings)
	if _, err := deps.Attempts.Advance(ctx, record.ID, attempt.Prepared, "exec", func(r *attempt.Record) { r.Admission = &admission }); err != nil {
		fail(err)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	if _, err := deps.Attempts.Advance(ctx, record.ID, attempt.Running, "exec", nil); err != nil {
		fail(err)
		return plan.StepResult{StartedAt: started, EndedAt: time.Now(), Error: err.Error()}, err
	}
	result, err := deps.Runner.RunStep(ctx, req)
	result.Agent, result.Node = candidate.Agent.ID, candidate.Node
	result.StartedAt, result.EndedAt = started, time.Now()
	if err != nil {
		result.Error = err.Error()
		fail(err)
		return result, err
	}
	// What the step made is published before it is judged: verification
	// runs against the same tree that will be bound.
	published, _, err := deps.Artifacts.Publish(ctx, workspace, base, record.ID, "step "+step.ID+" of plan "+p.ID)
	if err != nil {
		result.Error = "publish: " + err.Error()
		fail(err)
		return result, err
	}
	result.Artifact = published.ID
	// The declared write scope is enforced on what was actually written,
	// not on the declaration: a result that strayed does not bind.
	if strayed, err := outsideScope(ctx, deps, p.ProjectID, base, published.ID, step.Touches); err != nil {
		result.Error = err.Error()
		fail(err)
		return result, err
	} else if len(strayed) > 0 {
		err := fmt.Errorf("step %s wrote outside its declared paths: %s", step.ID, strings.Join(strayed, ", "))
		result.Error = err.Error()
		_, _ = deps.Attempts.Advance(context.WithoutCancel(ctx), record.ID, attempt.Failed, "exec", func(r *attempt.Record) { r.Error = err.Error() })
		_ = deps.Artifacts.Discard(context.WithoutCancel(ctx), workspace)
		return result, err
	}
	for _, to := range []attempt.State{attempt.Snapshotted, attempt.Published, attempt.Durable} {
		if _, err := deps.Attempts.Advance(ctx, record.ID, to, "exec", nil); err != nil {
			result.Error = err.Error()
			fail(err)
			return result, err
		}
	}
	defer func() { _ = deps.Artifacts.Discard(context.WithoutCancel(ctx), workspace) }()

	// An agent reporting success is not evidence of success. A step reaches
	// done only through its own verification.
	if step.Verify != nil && step.Verify.Kind != plan.VerifyNone {
		if deps.Verifier == nil {
			result.Error = "step requires verification but no verifier is wired"
			return result, fmt.Errorf("%s", result.Error)
		}
		if _, err := deps.Attempts.Advance(ctx, record.ID, attempt.Verifying, "exec", nil); err != nil {
			result.Error = err.Error()
			fail(err)
			return result, err
		}
		verifyErr := deps.Verifier.Verify(ctx, req, *step.Verify, result)
		// The verdict is a fact either way, written before it is acted on.
		if _, aerr := deps.Artifacts.Attest(ctx, attestationFor(p, step, record.ID, candidate.Node, published.ID, verifyErr)); aerr != nil {
			result.Error = "attest: " + aerr.Error()
			fail(aerr)
			return result, aerr
		}
		if err := verifyErr; err != nil {
			_, _ = deps.Attempts.Fail(context.WithoutCancel(ctx), record.ID, "exec", "verification failed: "+err.Error())
			result.Error = "verification failed: " + err.Error()
			return result, fmt.Errorf("%s", result.Error)
		}
	}
	result.Verified = true
	// A verified step binds only on a durable, passing attestation from
	// this very attempt: the bind trusts the record, not the call stack.
	if step.Verify != nil && step.Verify.Kind != plan.VerifyNone {
		attested, err := deps.Artifacts.Attested(ctx, published.ID, record.ID)
		if err != nil || !attested {
			err := fmt.Errorf("step %s: no durable passing attestation of %s on record", step.ID, published.ID[:12])
			result.Error = err.Error()
			fail(err)
			return result, err
		}
	}
	// Bound: the step's name points at its artifact, under compare-and-set
	// on the name's version, in the same breath as the attempt's last
	// transition. A name that moved underneath is a bind-conflict.
	name := stepRef(p.TaskID, step.ID)
	current, _, _ := deps.Artifacts.Resolve(ctx, name)
	if _, err := deps.Attempts.Advance(ctx, record.ID, attempt.BindReady, "exec", func(r *attempt.Record) {
		r.Result = &attempt.Result{Summary: clipSummary(result.Answer), Artifact: published.ID, Refs: refNames(result.Refs)}
	}); err != nil {
		result.Error = err.Error()
		fail(err)
		return result, err
	}
	if _, err := deps.Artifacts.Bind(ctx, name, current.Version, published.ID); err != nil {
		_, _ = deps.Attempts.Advance(context.WithoutCancel(ctx), record.ID, attempt.BindConflict, "exec", func(r *attempt.Record) { r.Error = err.Error() })
		result.Error = "bind: " + err.Error()
		return result, err
	}
	if _, err := deps.Attempts.Advance(ctx, record.ID, attempt.Bound, "exec", nil); err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.Refs = append(result.Refs, plan.Ref{Kind: "artifact", Value: published.ID})
	return result, nil
}

func place(ctx context.Context, step plan.Step, r *roster.Roster, p project.Project) (roster.Candidate, error) {
	if step.Agent != "" {
		for _, c := range r.All(ctx) {
			if c.Agent.ID != step.Agent {
				continue
			}
			if reason := admits(p, c); reason != "" {
				return roster.Candidate{}, ErrNowhereToRun{StepID: step.ID, Requires: step.Requires, Reasons: step.Agent + " is pinned but " + reason}
			}
			if !c.Eligible {
				return roster.Candidate{}, ErrNowhereToRun{
					StepID: step.ID, Requires: step.Requires,
					Reasons: step.Agent + " is pinned but " + c.Why,
				}
			}
			// Naming the agent narrows the choice; it does not waive the
			// step's requirements.
			if req, err := ability.Compile(step.Requires); err != nil {
				return roster.Candidate{}, ErrNowhereToRun{StepID: step.ID, Requires: step.Requires, Reasons: err.Error()}
			} else if m := c.Match(req); !m.OK() {
				return roster.Candidate{}, ErrNowhereToRun{StepID: step.ID, Requires: step.Requires, Reasons: step.Agent + " is pinned but " + m.Unmet()}
			}
			return c, nil
		}
		return roster.Candidate{}, ErrNowhereToRun{
			StepID: step.ID, Reasons: "unknown agent " + step.Agent,
		}
	}
	// Prefer someone who has not failed this step yet. But an exclusion
	// is a preference, not a rule: when the only machine that can do the
	// work is the one that just failed a check, the right move is to try
	// it again, not to declare the plan impossible. A flaky test and a
	// broken node look the same from here, and only a retry tells them
	// apart.
	candidates := admitted(p, r.Candidates(ctx, step.Requires, step.Tried))
	if len(candidates) == 0 {
		candidates = admitted(p, r.Candidates(ctx, step.Requires, nil))
	}
	if len(candidates) == 0 {
		reasons := r.Explain(ctx, step.Requires)
		for _, c := range r.Candidates(ctx, step.Requires, nil) {
			if reason := admits(p, c); reason != "" {
				reasons += "; " + c.Agent.ID + ": " + reason
			}
		}
		return roster.Candidate{}, ErrNowhereToRun{
			StepID: step.ID, Requires: step.Requires, Reasons: reasons,
		}
	}
	return candidates[0], nil
}

// ancestry is the chain of goals this step serves: the plan's goal, then the
// goals it depends on. Goals, never transcripts — the payload stays bounded
// and still says what the work is for.
func ancestry(p plan.Plan, step plan.Step) []string {
	out := []string{p.Goal}
	for _, id := range dependencies(step) {
		if dep, ok := p.Step(id); ok {
			out = append(out, dep.Goal)
		}
	}
	return out
}

// inherited collects what this step's dependencies produced. Steps exchange
// pointers and findings, never each other's transcripts.
func inherited(upstream []Result) ([]plan.Ref, []plan.Finding) {
	var refs []plan.Ref
	var findings []plan.Finding
	for _, item := range upstream {
		refs = append(refs, item.Result.Refs...)
		findings = append(findings, item.Result.Findings...)
	}
	return refs, findings
}

func deadlineLabel(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format("15:04")
}

// facts is what a step's agent is told about where it runs: its own
// machine's abilities for its harness, ids only, under a small budget.
// The rest of the fleet is a query away (steve_fleet), not a paragraph
// in every step's context.
func facts(self roster.Candidate, _ []roster.Candidate) []string {
	line := "this machine (" + nodewire.Place(self.Node) + "): " + ability.Compact(self.Snapshot, self.Harness, 12)
	if len(line) > factsBudget {
		line = line[:factsBudget] + "… (truncated; ask steve_fleet)"
	}
	return []string{line}
}

// factsBudget bounds the ability line inside a step's context.
const factsBudget = 4 << 10

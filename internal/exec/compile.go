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
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ctxpack"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/text"
)

// maxParallelSteps bounds how many steps of one plan run at once. A step
// spends nearly all its time waiting on an agent on another machine, so the
// ceiling is about not flooding the nodes, not about local CPU.
const maxParallelSteps = 8

// StepRequest is one step handed to whatever actually drives agents.
type StepRequest struct {
	PluginRuntime *plugins.RuntimeRef
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
	Project       string
	Workspace     string
	Goal          string
	Context       ctxpack.Context
	RecordSession func(string) error `json:"-"`
	OpenFailure   func(error) error  `json:"-"`
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
	Executions *execution.Registry
	Roster     *roster.Roster
	Runner     Runner
	Verifier   Verifier
	Budget     Budget
	Recorder   Recorder
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
			// Recovery happens here, inside the node, and not by failing
			// the run and retrying it. The runtime's failure semantics are
			// fail-fast — one node failing cancels the others — which is
			// right for a workflow whose nodes depend on each other and
			// wrong for a plan whose branches were placed on different
			// machines precisely because they do not. A branch that is
			// retrying must not take a healthy branch down with it.
			out, err := runStepWithRecovery(ctx, p, step, upstream, deps)
			if err == nil && out.PlanRevision == p.Rev {
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

// record projects the authoritative attempt outcome into the plan. A failed
// projection stops scheduling; recovery repairs it from the bound output.
func record(deps Deps, planID string, step plan.Step, result plan.StepResult, runErr error) error {
	if deps.Recorder == nil {
		return nil
	}
	step.Result = &result
	step.State = plan.StepDone
	if runErr != nil {
		step.State = plan.StepFailed
		var recovery *agentexec.RecoveryBlocked
		if errors.As(runErr, &recovery) || errors.Is(runErr, harness.ErrStopUnconfirmed) {
			step.State = plan.StepRunning
		}
	}
	if err := deps.Recorder.RecordStep(planID, step); err != nil {
		return fmt.Errorf("%w: step %s of plan %s: %w", ErrProjection, step.ID, planID, err)
	}
	return nil
}

// runStepWithRecovery tries a step until it succeeds or Steve gives up.
// Each attempt is recorded as it happens, so the plan store — and every
// screen that reads it — shows a step mid-retry rather than pending.
func runStepWithRecovery(ctx context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps) (plan.StepResult, error) {
	if restored, ok, err := restoreStep(ctx, p, &step, upstream, deps); err != nil {
		return restored, err
	} else if ok {
		return restored, record(deps, p.ID, step, restored, nil)
	}
	if deps.Attempts != nil {
		prior, found, err := deps.Attempts.LatestForTurn(ctx, p.ID+"/"+step.ID)
		if err != nil {
			return plan.StepResult{}, err
		}
		if found && retainedCandidate(prior) {
			return resumeRetainedStep(ctx, p, step, upstream, deps, prior)
		}
	}
	var last plan.StepResult
	var lastErr error
	for retry := step.Attempts; retry <= MaxRecoveries; retry++ {
		step.Attempts++
		out, err := runStep(ctx, p, step, upstream, deps)
		if saveErr := record(deps, p.ID, step, out, err); saveErr != nil {
			return out, saveErr
		}
		if err == nil {
			return out, nil
		}
		last, lastErr = out, err
		// Some failures do not get better by trying again.
		var retained *agentexec.RecoveryBlocked
		if errors.As(err, &retained) {
			return out, err
		}
		if ctx.Err() != nil || errors.Is(err, ErrCompletion) || errors.Is(err, harness.ErrStopUnconfirmed) || errors.Is(err, attempt.ErrStopConfirmationRequired) || errors.Is(err, task.ErrExecutionStopped) {
			return out, err
		}
		var nowhere ErrNowhereToRun
		var noBudget ErrNoBudget
		if errors.As(err, &nowhere) || errors.As(err, &noBudget) {
			return out, err
		}
		if out.Agent != "" && !slices.Contains(step.Tried, out.Agent) {
			step.Tried = append(step.Tried, out.Agent)
		}
		if retry < MaxRecoveries {
			if deps.recoveries != nil {
				deps.recoveries.Add(1)
			}
			slog.Warn(fmt.Sprintf("exec: step %q attempt %d on %s failed: %v — retrying", step.ID, step.Attempts, out.Agent, err), "plan", p.ID, "step", step.ID, "attempt", out.AttemptID, "task", p.TaskID, "node", out.Node)
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

// clipSummary is the step's answer as the record's summary: its first
// 200 runes.
func clipSummary(answer string) string { return text.Clip(answer, 200) }

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

// ErrCompletion means the result could not be committed. Its workspace is
// retained; retrying the agent would hide the original completion conflict.
var ErrCompletion = errors.New("execution result was not committed")

func attemptUsage(u *plan.Usage) *attempt.Usage {
	if u == nil {
		return nil
	}
	return &attempt.Usage{Model: u.Model, Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Context: u.Context, Reported: u.Reported}
}

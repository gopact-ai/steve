package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gopactsqlite "github.com/gopact-ai/gopact-ext/stores/sqlite"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

// --- fakes -------------------------------------------------------------

type fakeNodes struct {
	mu       sync.Mutex
	statuses []node.Status
}

func (f *fakeNodes) Statuses() []node.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]node.Status{}, f.statuses...)
}

func (*fakeNodes) EnsureConnected(context.Context) {}

func (f *fakeNodes) down(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.statuses {
		if f.statuses[i].Name == name {
			f.statuses[i].Up = false
			f.statuses[i].LastError = "connection reset"
		}
	}
}

type fakeRunner struct {
	mu          sync.Mutex
	seen        []StepRequest
	reply       func(StepRequest) (plan.StepResult, error)
	inFlight    int
	maxInFlight int
}

func (f *fakeRunner) RunStep(_ context.Context, req StepRequest) (plan.StepResult, error) {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	reply := f.reply
	f.mu.Unlock()

	// Long enough to overlap even with each step materialising its own
	// worktree first, which takes real git time.
	time.Sleep(250 * time.Millisecond)
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if reply != nil {
		return reply(req)
	}
	return plan.StepResult{Answer: "did " + req.StepID}, nil
}

func (f *fakeRunner) requests() []StepRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StepRequest{}, f.seen...)
}

func (f *fakeRunner) placedOn(stepID string) []string {
	var out []string
	for _, req := range f.requests() {
		if req.StepID == stepID {
			out = append(out, req.Node)
		}
	}
	return out
}

type verifyFunc func(StepRequest) error

func (f verifyFunc) Verify(_ context.Context, req StepRequest, _ plan.Verify, _ plan.StepResult) error {
	return f(req)
}

type budgetFunc func(string) (int, time.Time, error)

func (f budgetFunc) Reserve(taskID string) (int, time.Time, error) { return f(taskID) }

// --- fixtures ----------------------------------------------------------

func up(name string, caps ...string) node.Status {
	return node.Status{Name: name, Up: true, Advert: nodewire.Advert{
		Node: name, Capabilities: caps, Harnesses: []nodewire.Harness{{ID: "mock"}},
	}}
}

func testRoster(t *testing.T, nodes *fakeNodes) *roster.Roster {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":   {Harness: "mock", Default: true},
		"builder": {Harness: "mock", Node: "node-a"},
		"shipper": {Harness: "mock", Node: "node-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := roster.New(catalog)
	r.SetHubCapabilities([]string{"basic"})
	r.SetNodes(nodes)
	return r
}

func bothNodes() *fakeNodes {
	return &fakeNodes{statuses: []node.Status{
		up("node-a", "gpu", "basic", "work"),
		up("node-b", "prod-cred", "basic", "work"),
	}}
}

func step(id, goal string, requires []string, needs ...string) plan.Step {
	return plan.Step{
		ID: id, Goal: goal, Requires: requires, Needs: needs, State: plan.StepPending,
		Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
	}
}

func execute(t *testing.T, p plan.Plan, deps Deps) Outcome {
	t.Helper()
	runs := NewRuns(workflow.NewMemoryStore())
	outcome, _ := runs.Execute(t.Context(), p, deps)
	return outcome
}

// --- tests -------------------------------------------------------------

// A step's requirements decide which machine runs it.
func TestPlacementFollowsCapabilities(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "1", TaskID: "t1", Goal: "g", Steps: []plan.Step{
		step("gpu-work", "train", []string{"gpu"}),
		step("ship", "deploy", []string{"prod-cred"}, "gpu-work"),
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})

	if outcome.Err != nil {
		t.Fatalf("plan failed: %v", outcome.Err)
	}
	if got := runner.placedOn("gpu-work"); len(got) != 1 || got[0] != "node-a" {
		t.Errorf("gpu step ran on %v, want [node-a]", got)
	}
	if got := runner.placedOn("ship"); len(got) != 1 || got[0] != "node-b" {
		t.Errorf("prod step ran on %v, want [node-b]", got)
	}
}

// Fan-out really overlaps, and the merge waits for both branches.
func TestFanOutRunsInParallelAndMergeWaits(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "2", TaskID: "t2", Goal: "g", Steps: []plan.Step{
		step("seed", "prepare", []string{"basic"}),
		step("branch-a", "half one", []string{"gpu"}, "seed"),
		step("branch-b", "half two", []string{"prod-cred"}, "seed"),
		{
			ID: "merge", Goal: "converge", Requires: []string{"basic"},
			Merge: []string{"branch-a", "branch-b"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
		},
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})

	if outcome.Err != nil {
		t.Fatalf("plan failed: %v", outcome.Err)
	}
	if runner.maxInFlight < 2 {
		t.Errorf("fan-out never overlapped (max in flight = %d)", runner.maxInFlight)
	}
	order := []string{}
	for _, req := range runner.requests() {
		order = append(order, req.StepID)
	}
	if order[0] != "seed" || order[len(order)-1] != "merge" {
		t.Fatalf("order = %v; seed first, merge last", order)
	}
}

// A failed step is retried on a different machine, and — the reason for using
// a real workflow runtime — the steps that already succeeded are not re-run.
func TestRecoveryRetriesElsewhereAndReusesCheckpoints(t *testing.T) {
	art, att := stores(t)
	nodes := bothNodes()
	var failedOnce bool
	runner := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		if req.StepID == "work" && req.Node == "node-a" && !failedOnce {
			failedOnce = true
			nodes.down("node-a")
			return plan.StepResult{}, errors.New("node-a went away")
		}
		return plan.StepResult{Answer: "did " + req.StepID}, nil
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "3", TaskID: "t3", Goal: "g", Steps: []plan.Step{
		step("prep", "prepare", []string{"basic"}),
		step("work", "do it", []string{"work"}, "prep"),
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, nodes), Runner: runner})

	if outcome.Err != nil {
		t.Fatalf("recovery did not save the plan: %v", outcome.Err)
	}
	if outcome.Recoveries != 1 {
		t.Errorf("recoveries = %d, want 1", outcome.Recoveries)
	}
	if got := runner.placedOn("work"); len(got) != 2 || got[1] != "node-b" {
		t.Fatalf("work ran on %v, want the retry to land on node-b", got)
	}
	// The whole point: prep succeeded once and was not repeated.
	if got := runner.placedOn("prep"); len(got) != 1 {
		t.Fatalf("prep ran %d times; a completed step must be reused from its checkpoint", len(got))
	}
	t.Logf("work: %v, prep ran %d time(s)", runner.placedOn("work"), len(runner.placedOn("prep")))
}

// An agent claiming success is not enough; verification decides.
func TestVerificationIsRequiredNotAssumed(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "4", TaskID: "t4", Goal: "g", Steps: []plan.Step{{
		ID: "work", Goal: "do it", Requires: []string{"basic"}, State: plan.StepPending,
		Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: "make test"},
	}}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})

	if outcome.Err == nil {
		t.Fatal("a step requiring verification passed with no verifier wired")
	}
	if !strings.Contains(outcome.Err.Error(), "no verifier") {
		t.Fatalf("error = %v", outcome.Err)
	}
}

// Verification failure sends the step to another agent.
func TestVerifyFailureMovesTheStep(t *testing.T) {
	art, att := stores(t)
	var checked int
	deps := Deps{
		Workspaces: art, Attempts: att, Artifacts: art,
		Roster: testRoster(t, bothNodes()),
		Runner: &fakeRunner{},
		Verifier: verifyFunc(func(req StepRequest) error {
			checked++
			if checked == 1 {
				return errors.New("tests did not pass")
			}
			return nil
		}),
	}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "5", TaskID: "t5", Goal: "g", Steps: []plan.Step{{
		ID: "work", Goal: "do it", Requires: []string{"work"}, State: plan.StepPending,
		Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: "make test"},
	}}}, deps)

	if outcome.Err != nil {
		t.Fatalf("plan should have recovered after re-verification: %v", outcome.Err)
	}
	if outcome.Recoveries != 1 {
		t.Errorf("recoveries = %d, want 1", outcome.Recoveries)
	}
}

// A spent budget refuses before the work starts.
func TestBudgetRefusesAtAdmission(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "6", TaskID: "t6", Goal: "g", Steps: []plan.Step{
		step("work", "do it", []string{"basic"}),
	}}, Deps{
		Workspaces: art, Attempts: att, Artifacts: art,
		Roster: testRoster(t, bothNodes()), Runner: runner,
		Budget: budgetFunc(func(string) (int, time.Time, error) {
			return 0, time.Time{}, errors.New("task t6 budget exhausted: turns")
		}),
	})
	if outcome.Err == nil {
		t.Fatal("a step ran with no budget left")
	}
	if len(runner.requests()) != 0 {
		t.Fatal("the runner was called despite the budget being spent")
	}
	if !strings.Contains(outcome.Err.Error(), "budget exhausted") {
		t.Fatalf("error = %v", outcome.Err)
	}
}

// Nowhere to run names the reason and does not spin retrying.
func TestNowhereToRunNamesTheReasonAndStops(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "7", TaskID: "t7", Goal: "g", Steps: []plan.Step{
		step("exotic", "needs hardware nobody has", []string{"quantum"}),
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})

	if outcome.Err == nil {
		t.Fatal("an unplaceable step did not fail")
	}
	if !strings.Contains(outcome.Err.Error(), "quantum") || !strings.Contains(outcome.Err.Error(), "lacks") {
		t.Fatalf("error = %v, want the missing capability and who lacks it", outcome.Err)
	}
	if outcome.Recoveries != 0 {
		t.Errorf("retried %d times on a placement nothing can satisfy", outcome.Recoveries)
	}
	t.Logf("refused with: %v", outcome.Err)
}

// Recovery is bounded: a step that always fails stops rather than looping.
func TestRecoveryIsBounded(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{reply: func(StepRequest) (plan.StepResult, error) {
		return plan.StepResult{}, errors.New("always fails")
	}}
	done := make(chan Outcome, 1)
	go func() {
		done <- execute(t, plan.Plan{ProjectID: "p", ID: "8", TaskID: "t8", Goal: "g", Steps: []plan.Step{
			step("doomed", "never works", []string{"basic"}),
		}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})
	}()
	select {
	case outcome := <-done:
		if outcome.Err == nil {
			t.Fatal("a step that always fails reported success")
		}
		if outcome.Recoveries > MaxRecoveries {
			t.Fatalf("recovered %d times, past the cap", outcome.Recoveries)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("executor looped instead of giving up")
	}
}

// What the agent was given is assembled by the hub, from typed pieces.
func TestContextIsAssembledFromUpstream(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		if req.StepID == "build" {
			return plan.StepResult{
				Answer:   "built",
				Refs:     []plan.Ref{{Kind: "git", Value: "abc123", Note: "build output"}},
				Findings: []plan.Finding{{Text: "the config format changed in v2"}},
			}, nil
		}
		return plan.StepResult{Answer: "shipped"}, nil
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "9", TaskID: "t9", Goal: "ship the thing", Steps: []plan.Step{
		step("build", "compile it", []string{"gpu"}),
		step("ship", "release it", []string{"prod-cred"}, "build"),
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	var ship StepRequest
	for _, req := range runner.requests() {
		if req.StepID == "ship" {
			ship = req
		}
	}
	if !hasRef(ship.Context.Refs, "abc123") {
		t.Fatalf("refs did not reach the next step: %+v", ship.Context.Refs)
	}
	if len(ship.Context.Findings) != 1 {
		t.Fatalf("findings did not reach the next step: %+v", ship.Context.Findings)
	}
	if len(ship.Context.Ancestry) < 2 || ship.Context.Ancestry[0] != "ship the thing" {
		t.Fatalf("ancestry = %v, want the plan goal first", ship.Context.Ancestry)
	}
	if ship.Context.Bearings == "" {
		t.Fatal("no bearings: a cold-started agent has nothing to orient on")
	}
	if len(ship.Context.Facts) == 0 {
		t.Fatal("the agent was told nothing about the machine it landed on")
	}
	t.Logf("ship saw refs=%v findings=%d facts=%v",
		ship.Context.Refs, len(ship.Context.Findings), ship.Context.Facts)
}

// --- supervisor: the revision edge -------------------------------------

// scriptedPlanner returns canned revisions and records what it was asked.
type scriptedPlanner struct {
	revisions [][]plan.Step
	asked     []planner.Request
}

func (s *scriptedPlanner) Name() string { return "scripted" }
func (s *scriptedPlanner) Plan(_ context.Context, req planner.Request) (plan.Plan, error) {
	s.asked = append(s.asked, req)
	if req.Current.ID == "" {
		return plan.Plan{}, errors.New("scripted planner only revises")
	}
	if len(s.revisions) == 0 {
		return plan.Plan{}, errors.New("nothing better to propose")
	}
	next := s.revisions[0]
	s.revisions = s.revisions[1:]
	out := req.Current
	out.Steps = next
	return out, nil
}

func planStore(t *testing.T) *plan.Store {
	t.Helper()
	s, err := plan.Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A finding that invalidates later steps stops the run, produces a new
// revision with the reason on record, and the revised run reuses the step
// that already finished instead of running it again.
func TestFindingTakesTheRevisionEdgeAndKeepsFinishedWork(t *testing.T) {
	art, att := stores(t)
	plans := planStore(t)
	created, err := plans.Create(plan.Plan{ProjectID: "p", TaskID: "t", Goal: "ship", By: "test", Steps: []plan.Step{
		step("probe", "look at the target", []string{"gpu"}),
		step("ship", "release the old way", []string{"prod-cred"}, "probe"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		if req.StepID == "probe" {
			return plan.StepResult{
				Answer: "looked",
				Refs:   []plan.Ref{{Kind: "git", Value: "probe-sha"}},
				Findings: []plan.Finding{{
					Text: "the target now requires staging first", Invalidates: []string{"ship"},
				}},
			}, nil
		}
		return plan.StepResult{Answer: "did " + req.StepID}, nil
	}}
	revised := []plan.Step{
		step("probe", "look at the target", []string{"gpu"}),
		step("stage", "push through staging", []string{"prod-cred"}, "probe"),
		step("ship", "release from staging", []string{"prod-cred"}, "stage"),
	}
	p := &scriptedPlanner{revisions: [][]plan.Step{revised}}
	sup := NewSupervisor(p, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner}, nil)
	sup.SetPlans(plans)

	outcome, err := sup.Execute(t.Context(), created)
	if err != nil {
		t.Fatalf("plan should have recovered by revision: %v (recoveries=%d)", err, outcome.Recoveries)
	}
	// The planner saw the finding and the trigger.
	if len(p.asked) != 1 || !strings.Contains(p.asked[0].Trigger, "staging first") {
		t.Fatalf("planner was asked %d times with trigger %q", len(p.asked), triggerOf(p.asked))
	}
	// The revision is on record with its reason.
	final, _ := plans.Latest(created.ID)
	if final.Rev != 2 || !strings.Contains(final.Because, "staging first") {
		t.Fatalf("revision = rev %d because %q", final.Rev, final.Because)
	}
	// probe ran exactly once across both revisions; stage and ship ran.
	if got := runner.placedOn("probe"); len(got) != 1 {
		t.Fatalf("probe ran %d times; a finished step must be reused on revision", len(got))
	}
	if len(runner.placedOn("stage")) != 1 || len(runner.placedOn("ship")) != 1 {
		t.Fatalf("revised steps did not run: stage=%v ship=%v", runner.placedOn("stage"), runner.placedOn("ship"))
	}
	// And the reused step's refs reached the new step.
	var stage StepRequest
	for _, req := range runner.requests() {
		if req.StepID == "stage" {
			stage = req
		}
	}
	if !hasRef(stage.Context.Refs, "probe-sha") {
		t.Fatalf("the finished step's refs did not reach the revised step: %+v", stage.Context.Refs)
	}
}

// A placement nothing satisfies is a revision trigger too: a plan that
// needs "quantum" should be reshaped, not retried into the same wall.
func TestUnplaceableStepAsksForARevision(t *testing.T) {
	art, att := stores(t)
	plans := planStore(t)
	created, err := plans.Create(plan.Plan{ProjectID: "p", TaskID: "t", Goal: "g", By: "test", Steps: []plan.Step{
		step("exotic", "needs hardware nobody has", []string{"quantum"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := &scriptedPlanner{revisions: [][]plan.Step{{step("plain", "do it the ordinary way", []string{"gpu"})}}}
	sup := NewSupervisor(p, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: &fakeRunner{}}, nil)
	sup.SetPlans(plans)
	if _, err := sup.Execute(t.Context(), created); err != nil {
		t.Fatalf("the revision should have rescued the plan: %v", err)
	}
	final, _ := plans.Latest(created.ID)
	if final.Rev != 2 || !strings.Contains(final.Because, "quantum") {
		t.Fatalf("revision = rev %d because %q", final.Rev, final.Because)
	}
}

// Revisions are bounded, and when the planner has nothing better the
// original failure is what gets reported.
func TestRevisionIsBoundedAndHonest(t *testing.T) {
	art, att := stores(t)
	plans := planStore(t)
	created, _ := plans.Create(plan.Plan{ProjectID: "p", TaskID: "t", Goal: "g", By: "test", Steps: []plan.Step{
		step("exotic", "impossible", []string{"quantum"}),
	}})
	p := &scriptedPlanner{} // never has a better idea
	sup := NewSupervisor(p, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: &fakeRunner{}}, nil)
	sup.SetPlans(plans)
	_, err := sup.Execute(t.Context(), created)
	if err == nil || !strings.Contains(err.Error(), "quantum") {
		t.Fatalf("err = %v, want the original placement failure", err)
	}
	if len(p.asked) != 1 {
		t.Fatalf("planner asked %d times after refusing once", len(p.asked))
	}
}

func triggerOf(reqs []planner.Request) string {
	if len(reqs) == 0 {
		return ""
	}
	return reqs[len(reqs)-1].Trigger
}

// A FINDING is carried forward; only a REPLAN invalidates what follows. Real
// agents report plenty of the first kind, and re-planning on each one turns
// a working plan into churn.
func TestFindingCarriesForwardOnlyReplanInvalidates(t *testing.T) {
	answer := "done\nFINDING: the workspace is not a git repo\nREF: blob /w/out — output\nREPLAN: the API moved, the next steps target the wrong host"
	findings := parseFindings(answer)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if len(findings[0].Invalidates) != 0 || !strings.Contains(findings[0].Text, "git repo") {
		t.Fatalf("FINDING should carry forward without invalidating: %+v", findings[0])
	}
	if len(findings[1].Invalidates) == 0 || !strings.Contains(findings[1].Text, "API moved") {
		t.Fatalf("REPLAN should invalidate: %+v", findings[1])
	}
	if invalidated(plan.StepResult{Findings: findings[:1]}) != nil {
		t.Fatal("a plain FINDING took the replan edge")
	}
	if invalidated(plan.StepResult{Findings: findings}) == nil {
		t.Fatal("a REPLAN did not take the replan edge")
	}
}

// Duplicate dependencies never reach the graph even if validation is
// bypassed: the compiler dedupes so a join cannot count one node twice.
func TestCompilerDedupesDependencies(t *testing.T) {
	deps := dependencies(plan.Step{Needs: []string{"a", "b"}, Merge: []string{"a"}})
	if len(deps) != 2 || deps[0] != "a" || deps[1] != "b" {
		t.Fatalf("dependencies = %v, want [a b]", deps)
	}
}

// A branch that is retrying must not take a healthy branch down with it:
// the sibling runs exactly once and the plan still completes.
func TestSiblingSurvivesAnotherBranchRetrying(t *testing.T) {
	art, att := stores(t)
	var flaky int32
	runner := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		if req.StepID == "flaky" && atomic.AddInt32(&flaky, 1) == 1 {
			return plan.StepResult{}, errors.New("first try fails")
		}
		if req.StepID == "steady" {
			time.Sleep(80 * time.Millisecond) // still running when flaky fails
		}
		return plan.StepResult{Answer: "did " + req.StepID}, nil
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "s", TaskID: "ts", Goal: "g", Steps: []plan.Step{
		step("seed", "prepare", []string{"basic"}),
		step("flaky", "fails once", []string{"work"}, "seed"),
		step("steady", "slow and fine", []string{"work"}, "seed"),
		{
			ID: "merge", Goal: "converge", Requires: []string{"basic"},
			Merge: []string{"flaky", "steady"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
		},
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})
	if outcome.Err != nil {
		t.Fatalf("plan failed: %v", outcome.Err)
	}
	if got := runner.placedOn("steady"); len(got) != 1 {
		t.Fatalf("the healthy branch ran %d times; it was cancelled as a casualty", len(got))
	}
	if got := runner.placedOn("flaky"); len(got) != 2 {
		t.Fatalf("flaky ran %d times, want 2", len(got))
	}
	if outcome.Recoveries != 1 {
		t.Fatalf("recoveries = %d", outcome.Recoveries)
	}
}

// Giving up on a step is a revision trigger, and the error says which step
// and why — the planner needs both.
func TestExhaustedStepAsksForARevision(t *testing.T) {
	art, att := stores(t)
	runner := &fakeRunner{reply: func(StepRequest) (plan.StepResult, error) {
		return plan.StepResult{}, errors.New("always fails")
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "x", TaskID: "tx", Goal: "g", Steps: []plan.Step{
		step("doomed", "never works", []string{"basic"}),
	}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})
	var exhausted ErrExhausted
	if !errors.As(outcome.Err, &exhausted) || exhausted.StepID != "doomed" {
		t.Fatalf("err = %v, want ErrExhausted for doomed", outcome.Err)
	}
	if !NeedsRevision(outcome.Err) {
		t.Fatal("an exhausted step should ask for a revision")
	}
	if len(runner.placedOn("doomed")) != MaxRecoveries+1 {
		t.Fatalf("doomed ran %d times, want %d", len(runner.placedOn("doomed")), MaxRecoveries+1)
	}
}

// stores gives a test the ledger-backed pieces a step needs: a project "p"
// homed on the hub at a fresh directory, attempts, and artifacts whose
// node side runs on this machine.
func stores(t *testing.T) (*artifact.Store, *attempt.Service) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(context.Background(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	return artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()}), attempt.New(book)
}

// hasRef says whether a ref with the value reached a step. Steps also
// receive the artifact refs the executor adds, so exact lists are not the
// question.
func hasRef(refs []plan.Ref, value string) bool {
	for _, r := range refs {
		if r.Value == value {
			return true
		}
	}
	return false
}

// A step that writes outside its declared paths does not bind: the
// declaration is enforced on the diff, not trusted.
func TestResultOutsideDeclaredTouchesDoesNotBind(t *testing.T) {
	art, att := stores(t)
	writer := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		if err := os.WriteFile(filepath.Join(req.Workspace, "elsewhere.txt"), []byte("stray"), 0o644); err != nil {
			return plan.StepResult{}, err
		}
		return plan.StepResult{Answer: "wrote"}, nil
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "scope", TaskID: "tscope", Goal: "g", Steps: []plan.Step{{
		ID: "narrow", Goal: "only docs", Requires: []string{"basic"}, Touches: []string{"docs/"},
		State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
	}}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: writer})
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "outside its declared paths") {
		t.Fatalf("stray write was bound: %v", outcome.Err)
	}
	if _, ok, _ := art.Resolve(context.Background(), "steve/tscope/narrow"); ok {
		t.Fatal("the step's name was bound despite the scope violation")
	}
	// Within scope, the same writer binds.
	inside := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		_ = os.MkdirAll(filepath.Join(req.Workspace, "docs"), 0o755)
		return plan.StepResult{Answer: "wrote"}, os.WriteFile(filepath.Join(req.Workspace, "docs", "a.md"), []byte("ok"), 0o644)
	}}
	outcome = execute(t, plan.Plan{ProjectID: "p", ID: "scope2", TaskID: "tscope2", Goal: "g", Steps: []plan.Step{{
		ID: "narrow", Goal: "only docs", Requires: []string{"basic"}, Touches: []string{"docs/"},
		State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
	}}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: inside})
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	ref, ok, _ := art.Resolve(context.Background(), "steve/tscope2/narrow")
	if !ok || ref.Artifact == "" {
		t.Fatal("an in-scope result was not bound")
	}
}

// A retry takes the previous attempt over: the failed one is superseded and
// points at its successor, so the lineage of the step is on record.
func TestRetryTakesOverThePreviousAttempt(t *testing.T) {
	art, att := stores(t)
	var calls int
	runner := &fakeRunner{reply: func(req StepRequest) (plan.StepResult, error) {
		calls++
		if calls == 1 {
			return plan.StepResult{}, errors.New("first try broke")
		}
		return plan.StepResult{Answer: "second try worked"}, nil
	}}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "take", TaskID: "ttake", Goal: "g", Steps: []plan.Step{{
		ID: "work", Goal: "do", Requires: []string{"basic"},
		State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
	}}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: runner})
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	records, _ := att.ForTask(context.Background(), "ttake")
	if len(records) != 2 {
		t.Fatalf("attempts = %d, want the failed one and its successor", len(records))
	}
	first, second := records[0], records[1]
	if first.State != "superseded" || first.SupersededBy != second.ID || second.State != "bound" {
		t.Fatalf("first = %s (by %s), second = %s", first.State, first.SupersededBy, second.State)
	}
}

// Placement refuses a machine whose level does not reach the project's.
func TestPlacementRespectsTheProjectLevel(t *testing.T) {
	art, att := stores(t)
	book, _ := ledger.Open(t.TempDir(), ledger.Options{})
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(context.Background(), []project.Project{{ID: "p", Level: project.LevelRestricted, Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	art = artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir(), Levels: map[string]string{"node-a": "public"}})
	att = attempt.New(book)
	r := testRoster(t, bothNodes())
	r.SetNodeLevels(map[string]project.Level{"node-a": project.LevelPublic, "node-b": project.LevelRestricted})
	runner := &fakeRunner{}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "lvl", TaskID: "tlvl", Goal: "g", Steps: []plan.Step{{
		ID: "gpu-work", Goal: "needs the gpu box", Requires: []string{"gpu"},
		State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
	}}}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: r, Runner: runner})
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "public") {
		t.Fatalf("a restricted project was placed on a public node: %v", outcome.Err)
	}
	if len(runner.requests()) != 0 {
		t.Fatal("the step ran anyway")
	}
}

// The takeover drill: a hub dies mid-step. The next process resumes the
// run from its checkpoint, the step in flight is retried as a takeover of
// the attempt the dead process left live, and the plan finishes.
func TestResumeAfterCrashTakesOverTheLiveAttempt(t *testing.T) {
	art, att := stores(t)
	dbPath := filepath.Join(t.TempDir(), "workflows.db")
	if err := gopactsqlite.Migrate(dbPath); err != nil {
		t.Fatal(err)
	}
	store, err := gopactsqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	plans, err := plan.Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := plans.Create(plan.Plan{ProjectID: "p", TaskID: "tcrash", Goal: "survive", By: "test", Steps: []plan.Step{
		{ID: "first", Goal: "one", Requires: []string{"basic"}, State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"}},
		{ID: "second", Goal: "two", Requires: []string{"basic"}, Needs: []string{"first"}, State: plan.StepPending, Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Process one: the second step hangs until the process "dies".
	reached := make(chan struct{})
	book := att
	one := &Runs{store: store}
	ctx1, die := context.WithCancel(context.Background())
	deps1 := Deps{Workspaces: art, Attempts: book, Artifacts: art, Roster: testRoster(t, bothNodes()), Recorder: plans,
		Runner: runnerFunc(func(ctx context.Context, req StepRequest) (plan.StepResult, error) {
			if req.StepID == "second" {
				close(reached)
				<-ctx.Done()
				return plan.StepResult{}, ctx.Err()
			}
			return plan.StepResult{Answer: "did first"}, nil
		})}
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		_, _ = one.Execute(ctx1, created, deps1)
	}()
	<-reached
	// The hub dies: the step's attempt stays live in the ledger.
	die()
	<-done1
	live, _ := att.Live(context.Background())
	if len(live) != 1 || live[0].TurnID != created.ID+"/second" {
		// The cancelled step may have failed its attempt on the way out;
		// either way there must be a previous attempt of "second".
		prev, ok, _ := att.LatestForTurn(context.Background(), created.ID+"/second")
		if !ok {
			t.Fatalf("no attempt of the interrupted step on record (live=%v)", live)
		}
		t.Logf("previous attempt of second: %s", prev.State)
	}

	// Process two: same stores, fresh runtime, resume from the checkpoint.
	two := &Runs{store: store}
	calls := map[string]int{}
	deps2 := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Recorder: plans,
		Runner: runnerFunc(func(ctx context.Context, req StepRequest) (plan.StepResult, error) {
			calls[req.StepID]++
			return plan.StepResult{Answer: "did " + req.StepID}, nil
		})}
	latest, _ := plans.Latest(created.ID)
	outcome, err := two.Resume(context.Background(), latest, deps2, runIDFor(latest))
	if err != nil || outcome.Err != nil {
		t.Fatalf("resume = %v / %v", err, outcome.Err)
	}
	if calls["first"] != 0 || calls["second"] != 1 {
		t.Fatalf("resumed calls = %v; want only the interrupted step rerun (finished steps come from the plan store)", calls)
	}
	if got, _ := plans.Latest(created.ID); got.Steps[1].State != plan.StepDone || got.Steps[0].Result.Answer != "did first" {
		t.Fatalf("plan after resume = %+v", got.Steps)
	}
	records, _ := att.ForTask(context.Background(), "tcrash")
	var superseded, bound int
	for _, r := range records {
		if r.TurnID != created.ID+"/second" {
			continue
		}
		switch r.State {
		case "superseded":
			superseded++
		case "bound":
			bound++
		}
	}
	if superseded != 1 || bound != 1 {
		t.Fatalf("attempts of the interrupted step: %+v", records)
	}
}

// runnerFunc adapts a function to Runner.
type runnerFunc func(ctx context.Context, req StepRequest) (plan.StepResult, error)

func (f runnerFunc) RunStep(ctx context.Context, req StepRequest) (plan.StepResult, error) {
	return f(ctx, req)
}

// A verified step leaves a durable attestation behind, and binds only on it.
func TestVerifiedStepRecordsAnAttestation(t *testing.T) {
	art, att := stores(t)
	verdicts := 0
	deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Runner: &fakeRunner{},
		Verifier: verifyFunc(func(req StepRequest) error {
			verdicts++
			if verdicts == 1 {
				return errors.New("not yet")
			}
			return nil
		})}
	outcome := execute(t, plan.Plan{ProjectID: "p", ID: "att", TaskID: "tatt", Goal: "g", Steps: []plan.Step{{
		ID: "checked", Goal: "do", Requires: []string{"basic"}, State: plan.StepPending,
		Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: "make check"},
	}}}, deps)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	all, _ := art.Attestations(context.Background(), "")
	var pass, fail int
	for _, a := range all {
		if a.Step != "checked" || a.Kind != "command" || a.Verifier != "make check" || len(a.Receipts) == 0 {
			t.Fatalf("attestation = %+v", a)
		}
		switch a.Verdict {
		case "pass":
			pass++
		case "fail":
			fail++
		}
	}
	if pass != 1 || fail != 1 {
		t.Fatalf("attestations pass=%d fail=%d, want one of each", pass, fail)
	}
}

// Capacity: a single slot on node-a is reserved for the plan's steps up
// front; two parallel gpu steps share it in turn — the second waits for
// the slot instead of failing — and nothing stays reserved afterwards.
func TestReservedCapacityIsTakenOverAndWaitedFor(t *testing.T) {
	art, att := stores(t)
	slotPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { slotPollInterval = 3 * time.Second })
	nodes := &fakeNodes{statuses: []node.Status{{Name: "node-a", Up: true, Advert: nodewire.Advert{
		Node: "node-a", Capabilities: []string{"gpu", "basic"}, Harnesses: []nodewire.Harness{{ID: "mock", Slots: 1}},
	}}}}
	plans, err := plan.Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	sup := NewSupervisor(planner.Rule{}, Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, nodes), Runner: runner}, nil)
	sup.SetPlans(plans)
	created, err := plans.Create(plan.Plan{ProjectID: "p", TaskID: "tcap", Goal: "capacity", By: "test", Steps: []plan.Step{
		step("left", "half", []string{"gpu"}),
		step("right", "other half", []string{"gpu"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := sup.Execute(context.Background(), created)
	if err != nil || outcome.Err != nil {
		t.Fatalf("execute = %v / %v", err, outcome.Err)
	}
	if runner.maxInFlight != 1 {
		t.Fatalf("max in flight = %d; one slot must serialise the two steps", runner.maxInFlight)
	}
	for _, id := range []string{"left", "right"} {
		if _, ok, _ := att.ReservationFor(context.Background(), created.ID+"/"+id); ok {
			t.Fatalf("reservation for %s survived the plan", id)
		}
	}
	// One of the two attempts took a reservation over (its slot lease is at
	// a transferred epoch); the other waited for the slot to free.
	records, _ := att.ForTask(context.Background(), "tcap")
	transferred := 0
	for _, r := range records {
		for _, l := range r.Leases {
			if strings.HasPrefix(l.Key, "endpoint:node-a/mock:slot:") && l.Epoch >= 2 {
				transferred++
			}
		}
	}
	if transferred == 0 {
		t.Fatalf("no attempt took over a reserved slot: %+v", records)
	}
}

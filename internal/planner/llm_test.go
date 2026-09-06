package planner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
)

// scripted plays the planning agent: it answers each prompt in turn and
// records what it was asked, so a test can assert both the contract and the
// correction loop.
type scripted struct {
	answers []string
	asked   []string
	specs   []agentexec.Spec
	err     error
}

func (s *scripted) Prompt(_ context.Context, spec agentexec.Spec, text string, validate func(string) error) (agentexec.Result, error) {
	s.asked = append(s.asked, text)
	s.specs = append(s.specs, spec)
	if s.err != nil {
		return agentexec.Result{}, s.err
	}
	answer := ""
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	result := agentexec.Result{Answer: answer}
	if err := validate(answer); err != nil {
		return result, &agentexec.ValidationError{Cause: err}
	}
	return result, nil
}

func testRoster() []roster.Candidate {
	return []roster.Candidate{
		{Agent: agent.Agent{ID: "builder"}, Node: "node-a", Eligible: true, Capabilities: []string{"gpu"}},
		{Agent: agent.Agent{ID: "shipper"}, Node: "node-b", Eligible: true, Capabilities: []string{"prod-cred"}},
		{Agent: agent.Agent{ID: "sleeper"}, Node: "node-c", Eligible: false, Why: "node node-c is down"},
	}
}

const goodPlan = `Here is the plan:
{"steps":[
  {"id":"build","goal":"compile the release","requires":["gpu"],"verify":{"kind":"command","command":"make test"}},
  {"id":"stage","goal":"stage it","requires":["prod-cred"],"verify":{"kind":"none","why":"staging is idempotent"}},
  {"id":"ship","goal":"release","requires":["prod-cred"],"merge":["build","stage"],"verify":{"kind":"command","command":"curl -f https://x/health"}}
]}`

func TestLLMPlanIsParsedAndValidated(t *testing.T) {
	agentSession := &scripted{answers: []string{goodPlan}}
	p := LLM{Agent: "claude", Executor: agentSession}
	built, err := p.Plan(t.Context(), Request{Goal: "ship the release", TaskID: "7", Roster: testRoster(), TurnsLeft: 12})
	if err != nil {
		t.Fatal(err)
	}
	if built.By != "llm:claude" || built.TaskID != "7" || len(built.Steps) != 3 {
		t.Fatalf("plan = %+v", built)
	}
	ship, _ := built.Step("ship")
	if len(ship.Merge) != 2 || ship.Verify.Kind != plan.VerifyCommand {
		t.Fatalf("ship step = %+v", ship)
	}
	// Runtime state is never the model's to set.
	for _, s := range built.Steps {
		if s.State != plan.StepPending || s.Result != nil {
			t.Fatalf("model set runtime state on %s: %+v", s.ID, s)
		}
	}
	// The brief told it who is available and who is not, and why.
	brief := agentSession.asked[0]
	for _, want := range []string{"ship the release", "builder 在 node-a", "gpu", "sleeper", "node-c is down", "还剩 12 轮"} {
		if !strings.Contains(brief, want) {
			t.Errorf("planning brief is missing %q", want)
		}
	}
}

// A correction is a new bounded prompt with the previous output and exact error.
func TestInvalidPlanIsCorrectedNotAccepted(t *testing.T) {
	cyclic := `{"steps":[
	  {"id":"a","goal":"x","requires":["gpu"],"needs":["b"],"verify":{"kind":"none","why":"w"}},
	  {"id":"b","goal":"y","requires":["gpu"],"needs":["a"],"verify":{"kind":"none","why":"w"}}]}`
	agentSession := &scripted{answers: []string{cyclic, goodPlan}}
	p := LLM{Agent: "claude", Executor: agentSession}
	built, err := p.Plan(t.Context(), Request{Goal: "g", TaskID: "1", Roster: testRoster()})
	if err != nil {
		t.Fatalf("a correctable plan was not corrected: %v", err)
	}
	if len(built.Steps) != 3 {
		t.Fatalf("got the wrong plan: %d steps", len(built.Steps))
	}
	if len(agentSession.asked) != 2 || !strings.Contains(agentSession.asked[1], "cycle") {
		t.Fatalf("the correction did not name the problem: %q", agentSession.asked[len(agentSession.asked)-1])
	}
	if !strings.Contains(agentSession.asked[1], cyclic) || agentSession.specs[0].Kind != attempt.KindPlan || agentSession.specs[1].Kind != attempt.KindPlan || agentSession.specs[0].TurnID == agentSession.specs[1].TurnID {
		t.Fatal("correction lost its previous response or independent execution identity")
	}
}

func TestPlannerDoesNotRetryExecutionFailures(t *testing.T) {
	cause := errors.New("completion could not be recorded")
	executor := &scripted{err: cause}
	_, err := (LLM{Agent: "planner", Executor: executor}).Plan(t.Context(), Request{TaskID: "task", ProjectID: "project", Goal: "work"})
	if !errors.Is(err, cause) || len(executor.asked) != 1 {
		t.Fatalf("execution failure was retried: calls=%d err=%v", len(executor.asked), err)
	}
}

func TestUnverifiedStepIsRefused(t *testing.T) {
	lazy := `{"steps":[{"id":"a","goal":"x","requires":["gpu"]}]}`
	agentSession := &scripted{answers: []string{lazy, lazy}}
	p := LLM{Agent: "claude", Executor: agentSession, Attempts: 2}
	_, err := p.Plan(t.Context(), Request{Goal: "g", Roster: testRoster()})
	if err == nil {
		t.Fatal("a plan with an unverified step was accepted")
	}
	if !strings.Contains(err.Error(), "verified") {
		t.Fatalf("err = %v", err)
	}
}

func TestProseWithoutJSONIsRefused(t *testing.T) {
	agentSession := &scripted{answers: []string{"I would first build, then ship.", "still prose"}}
	p := LLM{Agent: "claude", Executor: agentSession, Attempts: 2}
	if _, err := p.Plan(t.Context(), Request{Goal: "g", Roster: testRoster()}); err == nil {
		t.Fatal("prose was accepted as a plan")
	}
}

// Re-planning keeps finished work: the model reshapes what comes next, it
// does not get to un-finish a step.
func TestRevisionKeepsDoneStepsAndTheirResults(t *testing.T) {
	current := plan.Plan{ID: "9", TaskID: "9", Rev: 1, Goal: "g", Steps: []plan.Step{
		{ID: "build", Goal: "compile", Requires: []string{"gpu"}, State: plan.StepDone, Attempts: 1,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "w"},
			Result: &plan.StepResult{Agent: "builder", Node: "node-a", Refs: []plan.Ref{{Kind: "git", Value: "abc"}}}},
		{ID: "ship", Goal: "release", Requires: []string{"prod-cred"}, Needs: []string{"build"}, State: plan.StepFailed,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "w"},
			Result: &plan.StepResult{Error: "prod-cred node is down",
				Findings: []plan.Finding{{Text: "the release must go through the staging box first"}}}},
	}}
	before := append([]plan.Step(nil), current.Steps...)
	revised := `{"steps":[
	  {"id":"build","goal":"compile","requires":["gpu"],"verify":{"kind":"none","why":"w"}},
	  {"id":"stage","goal":"push through staging","requires":["internal-net"],"needs":["build"],"verify":{"kind":"none","why":"w"}},
	  {"id":"ship","goal":"release from staging","requires":["prod-cred"],"needs":["stage"],"verify":{"kind":"none","why":"w"}}]}`
	agentSession := &scripted{answers: []string{revised}}
	p := LLM{Agent: "claude", Executor: agentSession}
	built, err := p.Plan(t.Context(), Request{
		Goal: "g", TaskID: "9", Current: current, Trigger: "ship failed: prod-cred node is down", Roster: testRoster(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if built.ID != "9" || built.Because != "ship failed: prod-cred node is down" {
		t.Fatalf("revision identity = %+v", built)
	}
	if !reflect.DeepEqual(current.Steps, before) {
		t.Fatal("planning mutated the prior revision while parsing a candidate")
	}
	build, _ := built.Step("build")
	if build.State != plan.StepDone || build.Result == nil || build.Result.Refs[0].Value != "abc" {
		t.Fatalf("finished step lost its result on revision: %+v", build)
	}
	if _, ok := built.Step("stage"); !ok {
		t.Fatal("the new step is missing")
	}
	ship, _ := built.Step("ship")
	if ship.State != plan.StepPending {
		t.Fatalf("the failed step should be pending again, got %s", ship.State)
	}
	// The brief carried the failure and the finding to the model.
	brief := agentSession.asked[0]
	for _, want := range []string{"上一版计划", "失败：prod-cred", "发现：the release must go through", "触发重规划"} {
		if !strings.Contains(brief, want) {
			t.Errorf("revision brief is missing %q", want)
		}
	}
}

package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
)

type retainedPlanning struct {
	scripted
	original agentexec.Spec
	answer   string
	resumed  []string
}

func (r *retainedPlanning) OriginalSpec(context.Context, string) (agentexec.Spec, error) {
	return r.original, nil
}
func (r *retainedPlanning) ResumeAttempt(_ context.Context, id string, validate func(string) error) (agentexec.Result, error) {
	r.resumed = append(r.resumed, id)
	out := agentexec.Result{Answer: r.answer}
	if err := validate(r.answer); err != nil {
		return out, &agentexec.ValidationError{Cause: err}
	}
	return out, nil
}

func TestRetainedPlanningUsesOriginalRequestAndAgent(t *testing.T) {
	first := &scripted{err: &agentexec.RecoveryBlocked{AttemptID: "original"}}
	_, _ = (LLM{Agent: "original-agent", Executor: first}).Plan(t.Context(), Request{Goal: "original goal", TaskID: "task", ProjectID: "project", Roster: testRoster(), TurnsLeft: 5})
	if len(first.specs) != 1 || !json.Valid(first.specs[0].Source) {
		t.Fatal("planning did not persist its original request")
	}
	recovered := &retainedPlanning{original: first.specs[0], answer: goodPlan}
	built, err := (LLM{Agent: "changed-agent", Executor: recovered}).ResumePlan(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	if built.Goal != "original goal" || built.TaskID != "task" || built.By != "llm:original-agent" || len(recovered.asked) != 0 || len(recovered.resumed) != 1 || recovered.resumed[0] != "original" {
		t.Fatalf("original planning was replayed or rebound: plan=%+v prompts=%v resumes=%v", built, recovered.asked, recovered.resumed)
	}
}

func TestRetainedInvalidPlanningCorrectsInNextOriginalRound(t *testing.T) {
	first := &scripted{err: &agentexec.RecoveryBlocked{AttemptID: "original"}}
	_, _ = (LLM{Agent: "original-agent", Executor: first, Attempts: 2}).Plan(t.Context(), Request{Goal: "original goal", TaskID: "task", ProjectID: "project", Roster: testRoster()})
	recovered := &retainedPlanning{scripted: scripted{answers: []string{goodPlan}}, original: first.specs[0], answer: "original invalid answer"}
	built, err := (LLM{Agent: "changed-agent", Executor: recovered, Attempts: 99}).ResumePlan(t.Context(), "original")
	if err != nil || len(built.Steps) != 3 {
		t.Fatalf("correction failed: plan=%+v err=%v", built, err)
	}
	if len(recovered.asked) != 1 || len(recovered.resumed) != 1 || recovered.specs[0].TurnID != "plan/task/r1/prompt/2" || recovered.specs[0].Agent != "original-agent" || !strings.HasPrefix(recovered.asked[0], first.asked[0]) || !strings.Contains(recovered.asked[0], recovered.answer) {
		t.Fatalf("correction replayed or rebuilt original input: prompts=%v specs=%+v", recovered.asked, recovered.specs)
	}
}

func TestRetainedPlanningRejectsMismatchedSource(t *testing.T) {
	first := &scripted{err: errors.New("detached")}
	_, _ = (LLM{Agent: "planner", Executor: first}).Plan(t.Context(), Request{Goal: "g", TaskID: "task", ProjectID: "project"})
	spec := first.specs[0]
	spec.TurnID = "plan/another-task/r1/prompt/1"
	recovered := &retainedPlanning{original: spec, answer: goodPlan}
	_, err := (LLM{Executor: recovered}).ResumePlan(t.Context(), "original")
	var blocked *agentexec.RecoveryBlocked
	if !errors.As(err, &blocked) || len(recovered.resumed) != 0 || len(recovered.asked) != 0 || blocked.AttemptID != "original" || recovered.original.Kind != attempt.KindPlan {
		t.Fatalf("mismatched source executed: err=%v resumed=%v", err, recovered.resumed)
	}
}

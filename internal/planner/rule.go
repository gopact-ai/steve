package planner

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/plan"
)

// Rule plans by matching what a step needs against what the roster offers.
// It does not decompose: a goal it was not given steps for becomes one step.
//
// That is not a placeholder for something cleverer. Most real routing is a
// capability question — this host reaches the build system, that one holds
// the production credentials — and answering it deterministically means the
// answer is inspectable, repeatable and free.
type Rule struct {
	// Workflows are decompositions the operator declared, keyed by a name
	// the goal can start with.
	Workflows map[string][]plan.Step
}

func (Rule) Name() string { return "rule" }

func (r Rule) Plan(_ context.Context, req Request) (plan.Plan, error) {
	if req.Current.ID != "" {
		return r.revise(req)
	}
	steps := r.decompose(req.Goal)
	built := plan.Plan{
		TaskID: req.TaskID, Goal: req.Goal, By: r.Name(),
		Because: firstNonEmpty(req.Trigger, "initial plan"),
		Steps:   steps,
	}
	if err := plan.Validate(built); err != nil {
		return plan.Plan{}, err
	}
	return built, nil
}

// decompose looks for a declared workflow whose name leads the goal; failing
// that, the goal is one step. A rule planner that invented a decomposition
// would be guessing, and a wrong guess costs more than a single step.
func (r Rule) decompose(goal string) []plan.Step {
	for name, steps := range r.Workflows {
		if matchesWorkflow(goal, name) {
			return cloneSteps(steps)
		}
	}
	return []plan.Step{{
		ID: "main", Goal: goal, Requires: []string{"any"}, State: plan.StepPending,
		Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "single-step goal from chat"},
	}}
}

// revise handles the executor's replan edge. The rule version is narrow on
// purpose: retry the failed step somewhere else, and give up when there is
// nowhere else. Anything more inventive belongs to a planner that can read.
func (r Rule) revise(req Request) (plan.Plan, error) {
	steps := cloneSteps(req.Current.Steps)
	changed := false
	for i := range steps {
		s := &steps[i]
		if s.State != plan.StepFailed {
			continue
		}
		// Remember who already failed so the retry lands somewhere new.
		if s.Result != nil && s.Result.Agent != "" && !contains(s.Tried, s.Result.Agent) {
			s.Tried = append(s.Tried, s.Result.Agent)
		}
		if !r.hasAlternative(req, *s) {
			// Leave it failed: retrying the same agent on the same machine
			// reproduces the same failure and spends the budget doing it.
			continue
		}
		s.State = plan.StepPending
		s.Agent = ""
		changed = true
	}
	if !changed {
		return plan.Plan{}, fmt.Errorf("nothing left to try: %s", req.Trigger)
	}
	revised := req.Current
	revised.Steps = steps
	revised.By = r.Name()
	revised.Because = req.Trigger
	if err := plan.Validate(revised); err != nil {
		return plan.Plan{}, err
	}
	return revised, nil
}

func (r Rule) hasAlternative(req Request, s plan.Step) bool {
	for _, c := range req.Roster {
		if !c.Eligible || contains(s.Tried, c.Agent.ID) {
			continue
		}
		if missing(s.Requires, c.Capabilities) {
			continue
		}
		return true
	}
	return false
}

func matchesWorkflow(goal, name string) bool {
	if name == "" || len(goal) < len(name) {
		return false
	}
	return equalFold(goal[:len(name)], name)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func missing(requires, have []string) bool {
	for _, want := range requires {
		if want == "" || want == "any" {
			continue
		}
		if !contains(have, want) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func cloneSteps(steps []plan.Step) []plan.Step {
	out := make([]plan.Step, len(steps))
	for i, s := range steps {
		copied := s
		copied.Needs = append([]string(nil), s.Needs...)
		copied.Merge = append([]string(nil), s.Merge...)
		copied.Requires = append([]string(nil), s.Requires...)
		copied.Tried = append([]string(nil), s.Tried...)
		if s.Verify != nil {
			v := *s.Verify
			copied.Verify = &v
		}
		if s.Result != nil {
			res := *s.Result
			res.Refs = append([]plan.Ref(nil), s.Result.Refs...)
			res.Findings = append([]plan.Finding(nil), s.Result.Findings...)
			copied.Result = &res
		}
		out[i] = copied
	}
	return out
}

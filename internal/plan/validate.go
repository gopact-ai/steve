package plan

import (
	"fmt"
	"github.com/gopact-ai/steve/internal/ability"
	"strings"
)

// MaxSteps bounds one plan. A planner that emits more than this has lost the
// thread, and executing it would spend the task's whole budget on scheduling.
const MaxSteps = 64

// Validate refuses a plan that cannot be executed, before any work starts.
// Every rule here is a failure the executor could not recover from: a cycle
// would deadlock, a dangling dependency would never become ready, and a step
// with no verification decision is exactly the case that ships untested work.
func Validate(p Plan) error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}
	if len(p.Steps) > MaxSteps {
		return fmt.Errorf("plan has %d steps, more than the %d a task may schedule", len(p.Steps), MaxSteps)
	}
	seen := make(map[string]bool, len(p.Steps))
	for _, s := range p.Steps {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("every step needs an id")
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Goal) == "" {
			return fmt.Errorf("step %q has no goal", s.ID)
		}
		// A step is placed by capability or by name, never by neither:
		// otherwise the executor has to guess which machine it meant.
		if s.Agent == "" && len(s.Requires) == 0 {
			return fmt.Errorf("step %q names neither an agent nor any requirement", s.ID)
		}
		if err := ability.ValidateText(s.Requires); err != nil {
			return fmt.Errorf("step %q: %w", s.ID, err)
		}
		if s.Verify == nil {
			return fmt.Errorf("step %q does not say how it is verified; use verify.kind=none with a reason to opt out", s.ID)
		}
		switch s.Verify.Kind {
		case VerifyCommand:
			if strings.TrimSpace(s.Verify.Command) == "" {
				return fmt.Errorf("step %q verifies by command but names none", s.ID)
			}
		case VerifyAgent:
			if strings.TrimSpace(s.Verify.Agent) == "" {
				return fmt.Errorf("step %q verifies by agent but names none", s.ID)
			}
		case VerifyNone:
			if strings.TrimSpace(s.Verify.Why) == "" {
				return fmt.Errorf("step %q opts out of verification without saying why", s.ID)
			}
		default:
			return fmt.Errorf("step %q has unknown verify kind %q", s.ID, s.Verify.Kind)
		}
	}
	for _, s := range p.Steps {
		listed := map[string]bool{}
		for _, dep := range append(append([]string{}, s.Needs...), s.Merge...) {
			if !seen[dep] {
				return fmt.Errorf("step %q depends on unknown step %q", s.ID, dep)
			}
			if dep == s.ID {
				return fmt.Errorf("step %q depends on itself", s.ID)
			}
			// A dependency listed twice — in needs and again in merge, say
			// — is two inbound edges from one node, and a join that counts
			// edges then fires before the other branch has arrived. Refuse
			// it here, where the reason can be sent back to the planner.
			if listed[dep] {
				return fmt.Errorf("step %q lists %q more than once across needs and merge; list each dependency once", s.ID, dep)
			}
			listed[dep] = true
		}
	}
	return cycleCheck(p)
}

// cycleCheck refuses a dependency loop. A cycle is not a slow plan, it is a
// plan that can never start: no step in the loop ever becomes ready.
func cycleCheck(p Plan) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[string]int, len(p.Steps))
	deps := make(map[string][]string, len(p.Steps))
	for _, s := range p.Steps {
		deps[s.ID] = append(append([]string{}, s.Needs...), s.Merge...)
	}
	var stack []string
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case grey:
			return fmt.Errorf("steps form a cycle: %s -> %s", strings.Join(stack, " -> "), id)
		case black:
			return nil
		}
		color[id] = grey
		stack = append(stack, id)
		for _, dep := range deps[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}
	for _, s := range p.Steps {
		if err := visit(s.ID); err != nil {
			return err
		}
	}
	return validateTouches(p)
}

// Covers says whether a declared path prefix covers a changed path. A
// declaration ending in "/" is a directory; anything else is one file.
func Covers(touches []string, path string) bool {
	for _, t := range touches {
		t = strings.TrimPrefix(t, "./")
		if t == path {
			return true
		}
		if strings.HasSuffix(t, "/") && strings.HasPrefix(path, t) {
			return true
		}
	}
	return false
}

// overlapping reports two declared scopes that can collide: parallel steps
// must not both claim a path, or one claim a directory the other's file
// sits in.
func overlapping(a, b []string) (string, bool) {
	for _, x := range a {
		for _, y := range b {
			x, y := strings.TrimPrefix(x, "./"), strings.TrimPrefix(y, "./")
			if x == y || Covers([]string{x}, y) || Covers([]string{y}, x) {
				return x, true
			}
		}
	}
	return "", false
}

// validateTouches refuses a plan whose independent steps declare
// overlapping paths. Steps ordered by Needs or Merge may share paths: they
// never run at once.
func validateTouches(p Plan) error {
	after := map[string]map[string]bool{}
	var ancestors func(id string, seen map[string]bool)
	byID := map[string]Step{}
	for _, s := range p.Steps {
		byID[s.ID] = s
	}
	ancestors = func(id string, seen map[string]bool) {
		s := byID[id]
		for _, dep := range append(append([]string{}, s.Needs...), s.Merge...) {
			if !seen[dep] {
				seen[dep] = true
				ancestors(dep, seen)
			}
		}
	}
	for _, s := range p.Steps {
		after[s.ID] = map[string]bool{}
		ancestors(s.ID, after[s.ID])
	}
	for i, a := range p.Steps {
		for _, b := range p.Steps[i+1:] {
			if after[a.ID][b.ID] || after[b.ID][a.ID] {
				continue
			}
			if path, clash := overlapping(a.Touches, b.Touches); clash {
				return fmt.Errorf("steps %q and %q may run at once and both touch %s", a.ID, b.ID, path)
			}
		}
	}
	return nil
}

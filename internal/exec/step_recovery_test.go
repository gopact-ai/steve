package exec

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/plan"
)

type rejectingRecorder struct {
	err   error
	saved []plan.Step
}

func (r *rejectingRecorder) RecordStep(_ string, step plan.Step) error {
	if r.err != nil {
		return r.err
	}
	r.saved = append(r.saved, step)
	return nil
}

func TestBoundOutputRestoresProjectionWithoutAnotherInvocation(t *testing.T) {
	art, att := stores(t)
	calls := 0
	cache := &rejectingRecorder{err: errors.New("projection write failed")}
	p := plan.Plan{ID: "p1", Rev: 1, ProjectID: "p", TaskID: "task"}
	step := step("build", "build", []string{"basic"})
	deps := Deps{Workspaces: art, Attempts: att, Artifacts: art, Roster: testRoster(t, bothNodes()), Recorder: cache, Runner: runnerFunc(func(context.Context, StepRequest) (plan.StepResult, error) {
		calls++
		return plan.StepResult{Answer: "full answer with every detail", Refs: []plan.Ref{{Kind: "git", Value: "ref", Note: "note"}}, Findings: []plan.Finding{{Text: "observation", Invalidates: []string{"later"}}}, Usage: &plan.Usage{Input: 100, Context: 500, Reported: true}}, nil
	})}
	first, err := runStepWithRecovery(t.Context(), p, step, nil, deps)
	if !errors.Is(err, ErrProjection) || calls != 1 {
		t.Fatalf("projection failure triggered execution retry: calls=%d err=%v", calls, err)
	}
	cache.err = nil
	restored, err := runStepWithRecovery(t.Context(), p, step, nil, deps)
	// JSON preserves instants, not monotonic readings or Location identity.
	first.StartedAt, first.EndedAt = first.StartedAt.UTC(), first.EndedAt.UTC()
	restored.StartedAt, restored.EndedAt = restored.StartedAt.UTC(), restored.EndedAt.UTC()
	if err != nil || calls != 1 || !reflect.DeepEqual(first, restored) || len(cache.saved) != 1 {
		t.Fatalf("bound output not restored: first=%+v restored=%+v calls=%d saved=%d err=%v", first, restored, calls, len(cache.saved), err)
	}
	changed := step
	changed.Goal = "different work"
	for _, rev := range []int{1, 2} {
		p.Rev = rev
		if _, err := runStepWithRecovery(t.Context(), p, changed, nil, deps); !errors.Is(err, ErrRecovery) || calls != 1 {
			t.Fatalf("changed completed spec silently executed: rev=%d calls=%d err=%v", rev, calls, err)
		}
	}
}

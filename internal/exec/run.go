package exec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/plan"
)

// Runs executes compiled plans.
//
// Step-level recovery lives inside each node (see runStepWithRecovery), so
// a run here either completes or fails because Steve gave up on a step. That
// is the moment the supervisor may revise the plan; it is not something a
// retry of the same run could fix.
type Runs struct {
	store workflow.Store

	mu    sync.Mutex
	sinks []gopact.EventSink
}

func NewRuns(store workflow.Store) *Runs { return &Runs{store: store} }

// Observe adds an event sink. The read model uses this to learn about step
// transitions when they happen rather than on its next poll.
func (r *Runs) Observe(sink gopact.EventSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sinks = append(r.sinks, sink)
}

// tracker remembers the run id, which only the event stream reveals.
type tracker struct {
	mu    sync.Mutex
	runID string
}

func (t *tracker) Emit(_ context.Context, e gopact.Event) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runID == "" {
		t.runID = e.RunID
	}
	return nil
}

// Outcome is how a plan finished, including where it got to when it did not.
type Outcome struct {
	Output Output
	RunID  string
	// Recoveries is how many step retries the run spent, across all steps.
	Recoveries int
	Err        error
}

// Execute compiles and runs the plan once. Retries happen inside the steps.
func (r *Runs) Execute(ctx context.Context, p plan.Plan, deps Deps) (Outcome, error) {
	return r.run(ctx, p, deps, gopact.WithRunID(runIDFor(p)))
}

func (r *Runs) run(ctx context.Context, p plan.Plan, deps Deps, extra ...gopact.RunOption) (Outcome, error) {
	track := &tracker{}
	r.mu.Lock()
	sinks := append([]gopact.EventSink{gopact.EventSink(track)}, r.sinks...)
	r.mu.Unlock()

	var recoveries atomic.Int64
	deps.recoveries = &recoveries
	wf, err := Compile(p, deps, r.store)
	if err != nil {
		return Outcome{}, err
	}
	options := make([]gopact.RunOption, 0, len(sinks)+len(extra))
	for _, sink := range sinks {
		options = append(options, gopact.WithEventSink(sink))
	}
	options = append(options, extra...)
	out, runErr := wf.Invoke(ctx, p.Goal, options...)
	return Outcome{Output: out, RunID: track.runID, Recoveries: int(recoveries.Load()), Err: runErr}, runErr
}

// NeedsRevision says whether a failed run is asking for a different plan.
// Every way a run can fail is one: a step that could not be placed, a step
// that found the plan wrong, or a step Steve gave up on. The runtime does
// not fail for anything less.
func NeedsRevision(err error) bool {
	if err == nil {
		return false
	}
	var inv ErrInvalidated
	var exhausted ErrExhausted
	var nowhere ErrNowhereToRun
	return errors.As(err, &inv) || errors.As(err, &exhausted) || errors.As(err, &nowhere) ||
		strings.Contains(err.Error(), "nothing can run it")
}

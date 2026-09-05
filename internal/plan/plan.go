// Package plan models how a goal gets broken into work and who does it.
//
// A plan is a persisted, versioned object rather than a value passed between
// function calls. That is not bookkeeping: a plan gets revised while it runs
// — a step fails, a node disappears, a step turns up a fact that invalidates
// what came after it — and the only way to answer "what changed, and why"
// afterwards is to have kept the revisions.
package plan

import "time"

// StepState tracks one step through the executor.
type StepState string

const (
	StepPending StepState = "pending"
	StepReady   StepState = "ready"
	StepRunning StepState = "running"
	// StepVerifying is a step whose work finished and whose verification is
	// still running. It is separate from done because an agent reporting
	// success is not evidence of success.
	StepVerifying StepState = "verifying"
	// StepAwaitingHuman is a step that needs an answer only a person can
	// give. The plan stops here rather than guessing — an interruption is
	// a state, not an error.
	StepAwaitingHuman StepState = "awaiting-human"
	StepDone          StepState = "done"
	StepFailed        StepState = "failed"
	StepSkipped       StepState = "skipped"
)

func (s StepState) Terminal() bool {
	return s == StepDone || s == StepFailed || s == StepSkipped
}

// VerifyKind is how a step's work is checked.
type VerifyKind string

const (
	VerifyCommand VerifyKind = "command"
	// VerifyAgent has a second agent check the first one's work.
	VerifyAgent VerifyKind = "agent"
	// VerifyNone must be written out. Agents mark work complete without
	// testing it, so "this step is unverified" has to be a visible choice
	// rather than the default that happens when nobody thought about it.
	VerifyNone VerifyKind = "none"
)

type Verify struct {
	Kind    VerifyKind `json:"kind"`
	Command string     `json:"command,omitempty"`
	Agent   string     `json:"agent,omitempty"`
	// Why records the reason when Kind is none, so a reviewer can judge it.
	Why string `json:"why,omitempty"`
}

// Ref is a pointer to state a step produced or needs — a commit, a blob, a
// task. Steps exchange refs rather than content: it keeps the context budget
// bounded and makes the exchange inspectable afterwards.
type Ref struct {
	Kind  string `json:"kind"` // "git" | "blob" | "task"
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

// Finding is something a step learned that later steps or the planner need.
// It is the input to re-planning: a step that discovers the API is different
// than assumed must be able to say so in a form the executor can act on.
type Finding struct {
	Text string `json:"text"`
	// Invalidates names steps this finding makes wrong, so re-planning has
	// somewhere concrete to start.
	Invalidates []string `json:"invalidates,omitempty"`
}

type StepResult struct {
	// Artifact is the step's published result: a commit in the project's
	// shadow repository, bound to the step's name.
	Artifact string    `json:"artifact,omitempty"`
	Answer   string    `json:"answer,omitempty"`
	Refs     []Ref     `json:"refs,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Node     string    `json:"node,omitempty"`
	TaskID   string    `json:"task_id,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Verified says whether the step's own check passed. A step can finish
	// its work and still not be done.
	Verified  bool      `json:"verified,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// Usage is what the step's turn cost and the model it ran on.
	Usage *Usage `json:"usage,omitempty"`
}

// Usage is a step's spend as the harness reported it.
type Usage struct {
	Model       string `json:"model,omitempty"`
	Input       int64  `json:"input,omitempty"`
	Output      int64  `json:"output,omitempty"`
	CachedRead  int64  `json:"cached_read,omitempty"`
	CachedWrite int64  `json:"cached_write,omitempty"`
}

// Step is one unit of work. Placement is expressed as Requires rather than a
// fixed agent wherever possible: the executor re-solves it before each run,
// which is what lets a plan survive a node going away mid-flight.
type Step struct {
	ID    string   `json:"id"`
	Goal  string   `json:"goal"`
	Needs []string `json:"needs,omitempty"`
	// Merge names the parallel branches this step converges. A workspace has
	// exactly one writer at a time, so fan-out has to be joined by a step
	// that owns the merge rather than by two agents editing one tree.
	Merge    []string `json:"merge,omitempty"`
	Requires []string `json:"requires,omitempty"`
	// Touches declares the paths the step will write, relative to the
	// workspace. Parallel steps may not overlap, and a step whose result
	// strays outside its declaration does not bind. Empty means the step
	// may touch anything in its own worktree.
	Touches  []string    `json:"touches,omitempty"`
	Agent    string      `json:"agent,omitempty"`
	Verify   *Verify     `json:"verify,omitempty"`
	State    StepState   `json:"state"`
	Attempts int         `json:"attempts,omitempty"`
	Result   *StepResult `json:"result,omitempty"`
	// Tried records agents already attempted, so a retry picks someone else
	// instead of repeating the same failure on the same machine.
	Tried []string `json:"tried,omitempty"`
}

// Plan is one revision of how a task will be done. Revisions accumulate;
// nothing is edited in place.
type Plan struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	// ProjectID is the project the task was created under. Every step of
	// every revision runs against that project; it is copied here so the
	// executor never has to look the task up to know where "here" is.
	ProjectID string `json:"project_id,omitempty"`
	// Base is the canonical snapshot the plan started from; every step's
	// workspace descends from it, and landing merges against it.
	Base  string `json:"base,omitempty"`
	Rev   int    `json:"rev"`
	Goal  string `json:"goal"`
	Steps []Step `json:"steps"`
	// Fixed says this plan is declared, not planned: a failed step ends
	// it rather than sending it back to a planner. A repair is one — the
	// step is the whole point, and a planner "improving" it would only
	// route around the machine that needs fixing.
	Fixed bool `json:"fixed,omitempty"`
	// By is the planner that produced this revision: "declared", "rule",
	// "llm:<agent>". Planning is an attributable act like any other.
	By string `json:"by"`
	// Because is what triggered this revision — the previous step that
	// failed, the finding, the node that vanished, the person who stepped
	// in. Rev 1 says how the plan started.
	Because   string    `json:"because"`
	CreatedAt time.Time `json:"created_at"`
}

// Step finds a step by id.
func (p Plan) Step(id string) (Step, bool) {
	for _, s := range p.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// Ready lists steps whose dependencies are all done and which have not run.
// It is the executor's whole scheduling rule: everything ready may run at
// once, which is what makes fan-out fall out of the DAG rather than needing
// its own mechanism.
func (p Plan) Ready() []Step {
	done := map[string]bool{}
	for _, s := range p.Steps {
		if s.State == StepDone || s.State == StepSkipped {
			done[s.ID] = true
		}
	}
	var ready []Step
	for _, s := range p.Steps {
		if s.State != StepPending && s.State != StepReady {
			continue
		}
		blocked := false
		for _, need := range append(append([]string{}, s.Needs...), s.Merge...) {
			if !done[need] {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, s)
		}
	}
	return ready
}

// Complete reports whether every step reached a terminal state.
func (p Plan) Complete() bool {
	for _, s := range p.Steps {
		if !s.State.Terminal() {
			return false
		}
	}
	return true
}

// Failed reports whether any step failed, which is what makes the task fail
// rather than quietly delivering partial work as if it were the answer.
func (p Plan) Failed() bool {
	for _, s := range p.Steps {
		if s.State == StepFailed {
			return true
		}
	}
	return false
}

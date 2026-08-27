// Package task models the unit of work a member is asked to complete. A task
// outlives any single turn: it survives session resets, member handoffs and
// gateway restarts, and it carries the budget that stops a runaway loop.
package task

import "time"

type State string

const (
	StateDraft   State = "draft"
	StateRunning State = "running"
	StateBlocked State = "blocked"
	StateReview  State = "review"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// transitions is deliberately permissive about re-entering running: a task
// takes many turns, and a blocked or failed task resumes rather than forking.
var transitions = map[State][]State{
	StateDraft:   {StateRunning, StateDone, StateFailed},
	StateRunning: {StateRunning, StateBlocked, StateReview, StateDone, StateFailed},
	StateBlocked: {StateRunning, StateFailed},
	StateReview:  {StateRunning, StateDone, StateFailed},
	StateDone:    {},
	StateFailed:  {StateRunning},
}

func (s State) CanMoveTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

func (s State) Terminal() bool { return s == StateDone }

type Outcome string

const (
	OutcomeOK        Outcome = "ok"
	OutcomeError     Outcome = "error"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeTimeout   Outcome = "timeout"
	// OutcomeInterrupted marks an attempt the gateway itself abandoned:
	// the process died mid-turn and closed the attempt on the next start.
	OutcomeInterrupted Outcome = "interrupted"
)

// Tokens is best-effort. Harnesses report usage in different shapes and some
// report none at all, so nothing that must work depends on it being populated.
type Tokens struct {
	Input       int64 `json:"input,omitempty"`
	Output      int64 `json:"output,omitempty"`
	CachedRead  int64 `json:"cached_read,omitempty"`
	CachedWrite int64 `json:"cached_write,omitempty"`
	Total       int64 `json:"total,omitempty"`
}

func (t Tokens) Add(other Tokens) Tokens {
	return Tokens{
		Input:       t.Input + other.Input,
		Output:      t.Output + other.Output,
		CachedRead:  t.CachedRead + other.CachedRead,
		CachedWrite: t.CachedWrite + other.CachedWrite,
		Total:       t.Total + other.Total,
	}
}

const (
	DefaultMaxTurns   = 24
	DefaultMaxElapsed = 45 * time.Minute
)

// Budget counts what Steve observes itself. Turns and elapsed time are the
// authoritative limits precisely because they never depend on a harness
// reporting usage; tokens ride along for display and cost attribution.
type Budget struct {
	Turns      int           `json:"turns"`
	MaxTurns   int           `json:"max_turns,omitempty"`
	ToolCalls  int           `json:"tool_calls,omitempty"`
	Elapsed    time.Duration `json:"elapsed,omitempty"`
	MaxElapsed time.Duration `json:"max_elapsed,omitempty"`
	Tokens     Tokens        `json:"tokens,omitzero"`
}

// Exhausted reports the first limit crossed, so the caller can say which one
// stopped the task rather than just that something did.
func (b Budget) Exhausted() (string, bool) {
	if b.MaxTurns > 0 && b.Turns >= b.MaxTurns {
		return "turns", true
	}
	if b.MaxElapsed > 0 && b.Elapsed >= b.MaxElapsed {
		return "elapsed", true
	}
	return "", false
}

// Attempt is one turn against the task. A task accumulates attempts across
// members and nodes, which is what makes a handoff inspectable after the fact.
type Attempt struct {
	Member    string    `json:"member"`
	Node      string    `json:"node,omitempty"`
	Session   string    `json:"session,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Outcome   Outcome   `json:"outcome,omitempty"`
	Tokens    Tokens    `json:"tokens,omitzero"`
}

func (a Attempt) Open() bool { return a.EndedAt.IsZero() }

type Task struct {
	ID        string `json:"id"`
	Goal      string `json:"goal"`
	Requester string `json:"requester,omitempty"`
	Channel   string `json:"channel"`
	Member    string `json:"member,omitempty"`
	Node      string `json:"node,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Where the task's turns anchor in the chat: enough to reply into the
	// right conversation (and topic) after a gateway restart.
	ChatID        string    `json:"chat_id,omitempty"`
	AnchorMessage string    `json:"anchor_message,omitempty"`
	ChatType      string    `json:"chat_type,omitempty"`
	Parent        string    `json:"parent,omitempty"`
	State         State     `json:"state"`
	Budget        Budget    `json:"budget,omitzero"`
	Attempts      []Attempt `json:"attempts,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

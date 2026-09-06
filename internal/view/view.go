// Package view holds the channel-neutral shape of one Steve turn: what
// happened, not how any particular chat surface draws it. Renderers depend on
// this package; nothing here may depend on a renderer.
package view

import "time"

type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type ToolStatus string

const (
	ToolRunning   ToolStatus = "running"
	ToolCompleted ToolStatus = "completed"
	ToolFailed    ToolStatus = "failed"
)

// Phase distinguishes "the agent process is still starting" from "the agent
// is working", so a cold npx start does not look like silent thinking.
type Phase string

const (
	PhaseWaking  Phase = "waking"
	PhaseRunning Phase = "running"
)

type Tool struct {
	ID string
	// Kind is the short tool name shown on the collapsed row; Name carries
	// the agent's full call description and moves inside the panel.
	Kind      string
	Name      string
	Detail    string
	Input     string
	Output    string
	Status    ToolStatus
	Children  []Tool
	StartedAt time.Time
	UpdatedAt time.Time
}

// StepStatus tracks one plan entry through the agent's own lifecycle.
type StepStatus string

const (
	StepPending    StepStatus = "pending"
	StepInProgress StepStatus = "in_progress"
	StepCompleted  StepStatus = "completed"
)

// Step is one entry of the plan the agent says it is working to. Agents send
// the whole plan on every revision, so a list of these replaces rather than
// merges.
type Step struct {
	Text   string
	Status StepStatus
}

type Usage struct {
	// Reported distinguishes an explicit zero-token report from no report.
	Reported         bool
	TotalTokens      uint64
	InputTokens      uint64
	OutputTokens     uint64
	CacheReadTokens  uint64
	CacheWriteTokens uint64
	// ThoughtTokens is kept separately because agents can already include
	// reasoning in OutputTokens; adding it would double-count that spend.
	ThoughtTokens uint64
	ContextTokens uint64
	ContextWindow uint64
	// Cost is the latest session total, not this turn's incremental spend.
	// Keeping its currency avoids assuming every provider bills in USD.
	Cost *Cost
}

// TokensReported also accepts positive counters supplied by a harness.
// Context occupancy alone is not a report of tokens spent.
func (u Usage) TokensReported() bool {
	return u.Reported || u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0
}

type Cost struct {
	Amount   float64
	Currency string
}

type Field struct {
	Label    string
	Value    string
	Wide     bool
	IsMetric bool
}

// Settings is how the agent says it is configured for this session: which
// model is answering, and which permission mode it is operating under. Both
// are the agent's to change mid-session, so they ride along with every
// progress snapshot instead of being read once when the session opens.
type Settings struct {
	Harness string
	// Adapter is the ACP agent's own name and version, from initialize.
	Adapter string
	Model   string
	// Models are the alternatives the agent offered for this session, by
	// the names a person would pick from. Empty when the harness does not
	// expose a model selector.
	Models []string
	Mode   string
	// Options are every selector the agent exposes for this session —
	// model, reasoning effort, mode, whatever it has — with the choice in
	// force and the choices on offer. Steve pins any of them per agent.
	Options []Option
	// Node is the machine the agent ran on; empty means the hub itself.
	// Placement belongs on the card's tail rather than in the chat's
	// addressing, so where an agent lives can change without every message
	// having to say so.
	Node string
}

func (s Settings) Empty() bool { return s.Harness == "" && s.Model == "" && s.Mode == "" }

// Span places narration, thought summaries and tool starts in arrival order.
// Tools are references: their changing status and output remain in Tools.
type Span struct {
	Kind string
	Text string
	Tool string
	At   time.Time
}

type Progress struct {
	// Agent is the agent id the turn runs as — scheduling context stamped
	// by whoever placed the turn, since a session knows only its
	// placement, not which agent it is for.
	Agent     string
	Answer    string
	Reasoning string
	Tools     []Tool
	Usage     Usage
	Settings  Settings
	Plan      []Step
	Timeline  []Span
}

type Approval struct {
	RequestID string
	ToolName  string
	Reason    string
}

type Turn struct {
	Title     string
	Status    Status
	Answer    string
	Reasoning string
	Error     string
	Fields    []Field
	Tools     []Tool
	Usage     Usage
	Plan      []Step
	Question  *Question
	// PlanHidden counts steps trimmed off the front of Plan so the card can
	// admit to the trim instead of quietly shortening the agent's plan.
	PlanHidden int
	Approval   *Approval
	Settings   Settings
	Phase      Phase
	TurnID     string
	// Recipient is the open id of the person this turn answers; the card
	// footer addresses them by name with a real mention.
	Recipient string
	// RecoverID names the conversation whose newest archived session this
	// card offers to restore; empty renders no restore button.
	RecoverID string
	StartedAt time.Time
	UpdatedAt time.Time
}

// Question is the agent asking the user to choose. Agents ask through ACP's
// elicitation, which permits arbitrary JSON-Schema forms; this is the subset
// Steve can actually put in front of a person as a card, and anything outside
// it is declined rather than half-rendered.
type Question struct {
	RequestID string
	Message   string
	Title     string
	Choices   []Choice
}

type Choice struct {
	Value  string
	Label  string
	Detail string
}

// Option is one selector an agent exposes: its id and name, the category
// it declares ("model", "mode", or its own), what it is set to now, and
// what it could be set to.
type Option struct {
	ID       string
	Name     string
	Category string
	Current  string
	Choices  []Choice
}

// Answer carries the chosen Choice.Value. An empty Value means the user
// declined or never answered.
type Answer struct {
	Value string
}

func (a Answer) Chosen() bool { return a.Value != "" }

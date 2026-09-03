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
	InputTokens      uint64
	OutputTokens     uint64
	CacheReadTokens  uint64
	CacheWriteTokens uint64
	ContextTokens    uint64
	ContextWindow    uint64
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
	Model   string
	// Models are the alternatives the agent offered for this session, by
	// the names a person would pick from. Empty when the harness does not
	// expose a model selector.
	Models []string
	Mode   string
	// Node is the machine the agent ran on; empty means the hub itself.
	// Placement belongs on the card's tail rather than in the chat's
	// addressing, so where an agent lives can change without every message
	// having to say so.
	Node string
}

func (s Settings) Empty() bool { return s.Harness == "" && s.Model == "" && s.Mode == "" }

type Progress struct {
	Answer    string
	Reasoning string
	Tools     []Tool
	Usage     Usage
	Settings  Settings
	Plan      []Step
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

// Answer carries the chosen Choice.Value. An empty Value means the user
// declined or never answered.
type Answer struct {
	Value string
}

func (a Answer) Chosen() bool { return a.Value != "" }

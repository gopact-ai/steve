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

type Progress struct {
	Answer    string
	Reasoning string
	Tools     []Tool
	Usage     Usage
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
	Approval  *Approval
	Phase     Phase
	TurnID    string
	StartedAt time.Time
	UpdatedAt time.Time
}

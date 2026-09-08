package consoleapi

import (
	"time"

	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type Progress struct {
	Phase     string     `json:"phase,omitempty"`
	Agent     string     `json:"agent,omitempty"`
	Node      string     `json:"node,omitempty"`
	Model     string     `json:"model,omitempty"`
	Reasoning string     `json:"reasoning,omitempty"`
	Answer    string     `json:"answer,omitempty"`
	Tools     []ToolCall `json:"tools,omitempty"`
	Plan      []PlanLine `json:"plan,omitempty"`
	Timeline  []Span     `json:"timeline,omitempty"`
}

type Span struct {
	Kind string    `json:"kind"`
	Text string    `json:"text,omitempty"`
	Tool string    `json:"tool,omitempty"`
	At   time.Time `json:"at"`
}

type ToolCall struct {
	ID     string          `json:"id,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	Name   string          `json:"name,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Status view.ToolStatus `json:"status"`
	Input  string          `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type PlanLine struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

type Process struct {
	Reasoning string        `json:"reasoning,omitempty"`
	Tools     []ToolCall    `json:"tools,omitempty"`
	Timeline  []Span        `json:"timeline,omitempty"`
	Steps     []StepProcess `json:"steps,omitempty"`
}

type StepProcess struct {
	ID        string     `json:"id"`
	Agent     string     `json:"agent,omitempty"`
	Node      string     `json:"node,omitempty"`
	Model     string     `json:"model,omitempty"`
	Reasoning string     `json:"reasoning,omitempty"`
	Tools     []ToolCall `json:"tools,omitempty"`
	Plan      []PlanLine `json:"plan,omitempty"`
	Timeline  []Span     `json:"timeline,omitempty"`
	// The rest is what a delegated child adds: what it was asked, how it
	// ended, what it said. A plan step leaves them empty.
	StepInfo
}

type StepInfo struct {
	Kind    string     `json:"kind,omitempty"`
	Goal    string     `json:"goal,omitempty"`
	State   task.State `json:"state,omitempty"`
	Since   string     `json:"since,omitempty"`
	Elapsed string     `json:"elapsed,omitempty"`
	Answer  string     `json:"answer,omitempty"`
	Refs    []string   `json:"refs,omitempty"`
	// Attempt and Files say what the child changed, once it ended.
	Attempt string `json:"attempt,omitempty"`
	Files   int    `json:"files,omitempty"`
}

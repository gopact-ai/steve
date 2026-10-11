package turnapi

import (
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/view"
)

// UserError is safe to show on its source channel. Presenters display Text verbatim.
type UserError struct {
	Text  string
	Cause error
}

func (e UserError) Error() string { return e.Text }

func (e UserError) Unwrap() error { return e.Cause }

// Injected is what a turn actually gave the agent, kept so "what did it
// see" can be answered from the record rather than recomputed from a
// configuration that may since have changed.
type Injected struct {
	Project, Workspace          string
	Agent, Node, Harness, Model string
	Options                     map[string]string
	Session                     string
	NewSession                  bool
	// InstructionsSent says the assembled instructions (identity, skills,
	// memory) went in front of this turn's prompt: at session start or
	// after an identity change. Instructions holds the text only when sent.
	InstructionsSent  bool
	Instructions      string
	InstructionsBytes int
	MCPServers        []string
	Fingerprint       string
	// Prompt is the text of this turn as sent, without the instructions.
	Prompt string
	// Sections is what the instructions sent this turn were made of.
	Sections []capability.Section
}

type Result struct {
	AgentID  string
	Title    string
	Text     string
	Activity []string
	Fields   []view.Field
	// Injected is what the agent was given for this turn.
	Injected *Injected
	// Attempt is the attempt the turn ran as; its record holds the
	// before and after snapshots.
	Attempt string
	// Recover marks a result whose card should offer to restore the
	// just-archived session — the /clear confirmation.
	Recover bool
}

// Package conversationapi defines transport-neutral conversation contracts.
package conversationapi

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/task"
	"time"
)

type QuoteRef struct {
	Conversation string `json:"conversation"`
	ReplyID      string `json:"reply_id"`
}

type ConversationInitializer interface {
	InitializeConversation(ctx context.Context, conversation, project string) error
}

type Service interface {
	Submissions
	Interactions
	ConversationInitializer
	ExchangeIdentity
	LocalizedVerbs
	// SubmissionCapabilities says which optional submission fields the
	// console honours, material references and interactive requests, so the
	// page can tell a hub that preserves them from one that only accepts them.
	SubmissionCapabilities() (materialRefs, interactiveRequests bool)
	Queue(conversation string) []Exchange
	DeleteQueued(id string) error
	EditQueued(id, input string) (Exchange, error)
	Steer(ctx context.Context, id string) (Exchange, error)
	Replies(conversation string) []Reply
	// Conversations names every console conversation with a transcript;
	// Summaries describes each one for a sidebar.
	Conversations() []string
	Summaries(ctx context.Context) []Conversation
	Update(ctx context.Context, conversation string, patch ConversationPatch) error
	// Context is where a conversation stands; VerbsFor is what it can be told.
	Context(ctx context.Context, conversation string) (Context, error)
	// Setup is what the conversation's agent works with: the assembled
	// instructions and the pieces they are made of.
	Setup(ctx context.Context, conversation, agent string) (Setup, error)
	// Suggest completes a line the page is typing, by the coordinator's
	// rules: verbs, agents, projects, this conversation's tasks.
	Suggest(ctx context.Context, conversation, line string) []Suggestion
}

type Suggestion struct {
	Label  string `json:"label"`
	Args   string `json:"args,omitempty"`
	Detail string `json:"detail,omitempty"`
	Insert string `json:"insert"`
	Muted  bool   `json:"muted,omitempty"`
}

type Context struct {
	Conversation string          `json:"conversation"`
	Project      *ContextProject `json:"project,omitempty"`
	Agent        *AgentChoice    `json:"agent,omitempty"`
	Agents       []AgentChoice   `json:"agents"`
}

type ContextProject struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Level   string `json:"level"`
	Repo    string `json:"repo"`
	Version int64  `json:"version"`
	Bound   bool   `json:"bound"`
}

type AgentChoice struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
	Ready   bool   `json:"ready"`
	Why     string `json:"why,omitempty"`
	Usable  bool   `json:"usable"`
	Because string `json:"because,omitempty"`
	Current bool   `json:"current,omitempty"`
	// Place is where this agent would work in the conversation's project:
	// the project's own rule, answered by the server, so no page has to
	// know it. Nil when the agent's machine has no workspace of it.
	Place *Placement `json:"place,omitempty"`
}

type Placement struct {
	Workspace string `json:"workspace"`
	Kind      string `json:"kind"`
	Node      string `json:"node"`
}

type Verb struct {
	Command string `json:"command"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary"`
}

type ChangeSummary struct {
	Attempt     string `json:"attempt"`
	Project     string `json:"project,omitempty"`
	Base        string `json:"base,omitempty"`
	Artifact    string `json:"artifact,omitempty"`
	Files       int    `json:"files"`
	Added       *int   `json:"added,omitempty"`
	Deleted     *int   `json:"deleted,omitempty"`
	BinaryFiles *int   `json:"binary_files,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Note        string `json:"note,omitempty"`
}

type Conversation struct {
	ID string `json:"id"`
	// Transport owns the opaque ID. Channel histories have no Console commands.
	Transport string `json:"transport,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	// Execution is channel processing, not task lifetime: running, idle, unknown.
	Execution string `json:"execution,omitempty"`
	// Title stays empty until non-command input or an explicit name supplies it.
	// Clients display a localized "New conversation" without persisting that placeholder.
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// Place is where the conversation's agent works in its project now.
	Place   *Placement `json:"place,omitempty"`
	LastAt  time.Time  `json:"last_at"`
	Count   int        `json:"count"`
	Running bool       `json:"running"`
	// TitleBy says who named it — "agent" or "user" — and Archived
	// whether the owner put it away.
	TitleBy  string `json:"title_by,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	// Questions counts what this thread is waiting for the owner to
	// answer, so a sidebar can mark the threads that need a person.
	Questions int `json:"questions,omitempty"`
}

type ConversationPatch struct {
	Title    *string `json:"title,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

type Reply struct {
	// ID names the line for good: a quote of it, a comment on it, a
	// process fetched for it later all point here rather than at a time.
	ID         string `json:"id,omitempty"`
	ExchangeID string `json:"exchange_id,omitempty"`
	// AttemptID is assigned from the server's execution result, never from a
	// submitted command. Durable delivery proofs use it independently of UI history.
	AttemptID    string    `json:"attempt_id,omitempty"`
	At           time.Time `json:"at"`
	Conversation string    `json:"conversation"`
	ProjectID    string    `json:"project_id,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	Input        string    `json:"input,omitempty"`
	Title        string    `json:"title,omitempty"`
	Text         string    `json:"text"`
	Format       string    `json:"format,omitempty"` // markdown (default) | text
	Error        string    `json:"error,omitempty"`
	Kind         string    `json:"kind"` // reply | milestone | notice
	// Silent keeps a line out of the transcript without keeping it out of
	// the ledger: stopping a turn is an act on the turn, not a thing said
	// in the conversation, so "/cancel" and its receipt are recorded and
	// auditable but never drawn as messages.
	Silent bool `json:"silent,omitempty"`
	// Relayed marks a sent line Steve put into the conversation on the
	// owner's behalf rather than one the owner typed: a schedule firing,
	// a delegated task reporting back. It reads like a message but it is
	// a record of something that happened, so it cannot be rewritten.
	Relayed bool `json:"relayed,omitempty"`
	// Delivery distinguishes a retained channel result from confirmed delivery.
	// Values are confirmed, unconfirmed, suppressed, or unavailable.
	Delivery string `json:"delivery,omitempty"`
	// Process is how the reply was made, for the page to unfold; Injected
	// what the agent was given for the turn.
	Process  *Process  `json:"process,omitempty"`
	Injected *Injected `json:"injected,omitempty"`
	// Changes is what the turn changed, when an attempt captured it.
	Changes   *ChangeSummary    `json:"changes,omitempty"`
	Refs      []material.Ref    `json:"refs,omitempty"`
	Materials []material.Frozen `json:"materials,omitempty"`
}

type Setup struct {
	Agent        string    `json:"agent"`
	Node         string    `json:"node,omitempty"`
	Harness      string    `json:"harness"`
	Model        string    `json:"model,omitempty"`
	Mode         string    `json:"mode,omitempty"`
	Instructions string    `json:"instructions,omitempty"`
	Sections     []Section `json:"sections,omitempty"`
	MCPServers   []string  `json:"mcp_servers,omitempty"`
	Applied      bool      `json:"applied"`
}

type Section struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes"`
}

type Injected struct {
	Project           string            `json:"project,omitempty"`
	Workspace         string            `json:"workspace,omitempty"`
	Agent             string            `json:"agent"`
	Node              string            `json:"node,omitempty"`
	Harness           string            `json:"harness"`
	Model             string            `json:"model,omitempty"`
	Options           map[string]string `json:"options,omitempty"`
	Session           string            `json:"session,omitempty"`
	NewSession        bool              `json:"new_session"`
	InstructionsSent  bool              `json:"instructions_sent"`
	Instructions      string            `json:"instructions,omitempty"`
	InstructionsBytes int               `json:"instructions_bytes"`
	MCPServers        []string          `json:"mcp_servers,omitempty"`
	Fingerprint       string            `json:"fingerprint,omitempty"`
	Prompt            string            `json:"prompt,omitempty"`
	// Sections is what the instructions were made of, when they were
	// sent: the same breakdown the setup view shows.
	Sections []Section `json:"sections,omitempty"`
}

type Exchange struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Input        string `json:"input"`
	// Prompt is what the agent is given when it differs from Input: a
	// continuation after a restart shows the notice and says "go on".
	Prompt string `json:"prompt,omitempty"`
	// History is what the thread said before this line, carried as text
	// because the agent's own session no longer holds it: a thread rewound
	// to an edited message answers from a session that never saw the turns
	// that were removed.
	History         string               `json:"history,omitempty"`
	Origin          string               `json:"origin,omitempty"`
	Requester       string               `json:"requester,omitempty"`
	ExpectedProject string               `json:"expected_project,omitempty"`
	ExpectedTask    string               `json:"expected_task,omitempty"`
	ResumeAdmission task.ResumeAdmission `json:"resume_admission,omitzero"`
	// Key is the durable submission identity within this conversation.
	// Client command IDs and platform deliveries have separate namespaces.
	Key        string            `json:"key,omitempty"`
	Quotes     []QuoteRef        `json:"quotes,omitempty"`
	Refs       []material.Ref    `json:"refs,omitempty"`
	Materials  []material.Frozen `json:"materials,omitempty"`
	Locale     string            `json:"locale,omitempty"`
	State      ExchangeState     `json:"state"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
	StartedAt  time.Time         `json:"started_at,omitzero"`
	ReplyID    string            `json:"reply_id,omitempty"`
	// StopTarget is the exchange a targeted /cancel control stops.
	StopTarget string `json:"stop_target,omitempty"`
}

var (
	ErrExchangeNotFound  = errors.New("exchange not found")
	ErrExchangeNotQueued = errors.New("exchange is no longer queued")
	ErrCommandConflict   = errors.New("command_id was already used with a different submission")
	// ErrRewindTargetGone is a line a submission meant to replace that is
	// no longer in the transcript: it was already rewound past, or the
	// thread was trimmed beyond it.
	ErrRewindTargetGone = errors.New("that message is no longer in this thread")
	// ErrStopTargetEnded refuses a new stop for an exchange that has ended.
	ErrStopTargetEnded = errors.New("the turn to stop has already ended")
)

// ErrProjectConflict matches a ProjectConflictError.
var ErrProjectConflict = errors.New("project conflict")

// ProjectConflictError refuses a submission whose expected project is not
// the project the conversation is bound to. Nothing was queued.
type ProjectConflictError struct{ Expected, Bound string }

func (e ProjectConflictError) Error() string {
	return "expected project " + e.Expected + ", but conversation is bound to " + e.Bound
}

func (e ProjectConflictError) Is(target error) bool { return target == ErrProjectConflict }

var ErrBusy = errors.New("busy")

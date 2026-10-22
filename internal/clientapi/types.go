// Package clientapi serves steve.client.v1, the versioned protocol every
// Steve client (terminal, desktop, web console) speaks to one server.
//
// The package adapts the console and conversation services; it owns no
// state. Types here are the wire contract. Changes are additive only: a
// field or enum value may be added, never renamed, retyped or removed, and
// clients ignore what they do not know. docs/protocol/steve.client.v1.schema.json
// is generated from these types and a test fails when it is stale.
package clientapi

import "time"

// Protocol is the name and version clients negotiate with GET /v1/initialize.
const Protocol = "steve.client.v1"

// TurnState is where one exchange stands.
type TurnState string

const (
	TurnQueued    TurnState = "queued"
	TurnRunning   TurnState = "running"
	TurnDone      TurnState = "done"
	TurnFailed    TurnState = "failed"
	TurnCancelled TurnState = "cancelled"
)

// ItemKind is what one entry of a thread's transcript is.
type ItemKind string

const (
	ItemMessage    ItemKind = "message"
	ItemReasoning  ItemKind = "reasoning"
	ItemToolCall   ItemKind = "tool_call"
	ItemFileChange ItemKind = "file_change"
	ItemQuestion   ItemKind = "question"
	ItemMilestone  ItemKind = "milestone"
	ItemNotice     ItemKind = "notice"
)

// EventType classifies a change on the event stream.
type EventType string

const (
	EventThreadUpdated   EventType = "thread.updated"
	EventThreadDeleted   EventType = "thread.deleted"
	EventTurnUpdated     EventType = "turn.updated"
	EventTurnProgress    EventType = "turn.progress"
	EventItemCompleted   EventType = "item.completed"
	EventItemRemoved     EventType = "item.removed"
	EventQuestionUpdated EventType = "question.updated"
	EventTaskUpdated     EventType = "task.updated"
	EventServer          EventType = "server.event"
)

// Initialize is what a client learns before anything else.
type Initialize struct {
	Protocol      string   `json:"protocol"`
	ServerVersion string   `json:"server_version"`
	Capabilities  []string `json:"capabilities"`
	// Locale is the language the server answers this client in.
	Locale string `json:"locale"`
}

// Thread is one conversation.
type Thread struct {
	ID string `json:"id"`
	// Title is empty until the thread is named; clients show their own
	// placeholder.
	Title string `json:"title"`
	// Transport is console for threads clients can act in; any other
	// transport is a read-only channel history.
	Transport        string    `json:"transport"`
	ReadOnly         bool      `json:"read_only"`
	Project          string    `json:"project,omitempty"`
	Agent            string    `json:"agent,omitempty"`
	Running          bool      `json:"running"`
	Archived         bool      `json:"archived"`
	PendingQuestions int       `json:"pending_questions"`
	Lines            int       `json:"lines"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ThreadList struct {
	Threads []Thread `json:"threads"`
}

// ThreadDetail is a thread with its live turns and one page of items.
type ThreadDetail struct {
	Thread Thread `json:"thread"`
	Turns  []Turn `json:"turns"`
	// Items are oldest first. NextCursor reads the page before this one;
	// it is empty when the page reaches the start of the thread.
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// CreateThread binds a new thread to a project and, optionally, an agent.
type CreateThread struct {
	// ID is chosen by the client so a retry names the same thread; the
	// server prefixes it with "console:" when it lacks the prefix and
	// generates one when it is empty.
	ID      string `json:"id,omitempty"`
	Project string `json:"project"`
	Agent   string `json:"agent,omitempty"`
}

// Turn is one exchange: an input and what became of it.
type Turn struct {
	ID     string    `json:"id"`
	Thread string    `json:"thread"`
	State  TurnState `json:"state"`
	// Phase refines a running turn: recovering or awaiting-user.
	Phase           string    `json:"phase,omitempty"`
	Input           string    `json:"input"`
	CommandID       string    `json:"command_id,omitempty"`
	ExpectedProject string    `json:"expected_project,omitempty"`
	Origin          string    `json:"origin,omitempty"`
	QueuedAt        time.Time `json:"queued_at"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	// ReplyItem is the item that answered the turn once it ended.
	ReplyItem string `json:"reply_item,omitempty"`
}

// StartTurn submits input to a thread. CommandID is the idempotency key:
// repeating it with the same input returns the same turn; with different
// input it is refused.
type StartTurn struct {
	Thread    string `json:"thread"`
	Input     string `json:"input"`
	CommandID string `json:"command_id"`
	// ExpectedProject refuses the turn when the thread is bound elsewhere.
	ExpectedProject string        `json:"expected_project,omitempty"`
	Quotes          []Quote       `json:"quotes,omitempty"`
	Refs            []MaterialRef `json:"refs,omitempty"`
	Locale          string        `json:"locale,omitempty"`
	// RewindTo replaces an earlier sent item and everything after it.
	RewindTo string `json:"rewind_to,omitempty"`
}

// Quote carries one item of a thread along with the input. Only an
// assistant message, a milestone or a notice can be quoted; a user
// message, a question or an item derived from a reply (an ID with "#") is
// refused with bad_request.
type Quote struct {
	Thread string `json:"thread"`
	Item   string `json:"item"`
}

// MaterialRef attaches a stored material, optionally a part of it.
type MaterialRef struct {
	ID       string            `json:"id"`
	Selector *MaterialSelector `json:"selector,omitempty"`
}

type MaterialSelector struct {
	// Kind is lines (1-based, inclusive), quote (exact text) or rect.
	Kind  string        `json:"kind"`
	Start int           `json:"start,omitempty"`
	End   int           `json:"end,omitempty"`
	Quote string        `json:"quote,omitempty"`
	Rect  *MaterialRect `json:"rect,omitempty"`
}

type MaterialRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Interrupt stops a turn. CommandID keys the stop request sent for a
// running turn.
type Interrupt struct {
	CommandID string `json:"command_id"`
}

// InterruptResult is the turn as it stood and, for a running turn, the
// control turn that stops it.
type InterruptResult struct {
	Turn    Turn  `json:"turn"`
	Control *Turn `json:"control,omitempty"`
}

// Item is one entry of a thread's transcript.
type Item struct {
	ID     string    `json:"id"`
	Kind   ItemKind  `json:"kind"`
	Thread string    `json:"thread"`
	Turn   string    `json:"turn,omitempty"`
	At     time.Time `json:"at"`
	// Role is user or assistant for a message.
	Role   string `json:"role,omitempty"`
	Text   string `json:"text,omitempty"`
	Format string `json:"format,omitempty"`
	Title  string `json:"title,omitempty"`
	Error  string `json:"error,omitempty"`
	// Relayed marks a user message Steve sent on the owner's behalf.
	Relayed  bool        `json:"relayed,omitempty"`
	Attempt  string      `json:"attempt,omitempty"`
	Tool     *ToolCall   `json:"tool,omitempty"`
	Change   *FileChange `json:"change,omitempty"`
	Question *Question   `json:"question,omitempty"`
}

// ToolCall is one tool an agent used, or one delegated step.
type ToolCall struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Status is running, completed or failed.
	Status string `json:"status"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Node   string `json:"node,omitempty"`
	Task   string `json:"task,omitempty"`
}

// FileChange summarizes what a turn's attempt changed.
type FileChange struct {
	Attempt   string `json:"attempt"`
	Project   string `json:"project,omitempty"`
	Files     int    `json:"files"`
	Added     *int   `json:"added,omitempty"`
	Deleted   *int   `json:"deleted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Question is something an agent waits for the owner to decide.
type Question struct {
	ID      string `json:"id"`
	Thread  string `json:"thread"`
	Turn    string `json:"turn"`
	Project string `json:"project,omitempty"`
	Task    string `json:"task,omitempty"`
	Attempt string `json:"attempt,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Node    string `json:"node,omitempty"`
	// Kind is permission or question.
	Kind          string           `json:"kind"`
	Title         string           `json:"title,omitempty"`
	Message       string           `json:"message"`
	Options       []QuestionOption `json:"options"`
	AllowFreeText bool             `json:"allow_free_text"`
	Required      bool             `json:"required"`
	// State is pending, answered, declined, cancelled, expired or interrupted.
	State     string          `json:"state"`
	CreatedAt time.Time       `json:"created_at"`
	Deadline  time.Time       `json:"deadline,omitzero"`
	Answer    *QuestionAnswer `json:"answer,omitempty"`
}

type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

// QuestionAnswer is a decision: accept, decline or cancel. CommandID makes
// it idempotent; a different decision for a resolved question is refused.
type QuestionAnswer struct {
	CommandID string `json:"command_id"`
	Decision  string `json:"decision"`
	Choice    string `json:"choice,omitempty"`
	Text      string `json:"text,omitempty"`
}

type AnswerResult struct {
	Question Question `json:"question"`
}

// AttemptDiff is an attempt's changed files, or one file's unified diff
// when a path is asked for.
type AttemptDiff struct {
	Attempt   string        `json:"attempt"`
	Project   string        `json:"project,omitempty"`
	Path      string        `json:"path,omitempty"`
	Diff      string        `json:"diff,omitempty"`
	Files     []ChangedFile `json:"files,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Note      string        `json:"note,omitempty"`
}

type ChangedFile struct {
	Path string `json:"path"`
	// Status is A, M or D.
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

type Project struct {
	ID      string   `json:"id"`
	Node    string   `json:"node"`
	Path    string   `json:"path"`
	Repo    string   `json:"repo,omitempty"`
	Level   string   `json:"level"`
	Home    bool     `json:"home,omitempty"`
	Default bool     `json:"default,omitempty"`
	Agents  []string `json:"agents"`
}

type ProjectList struct {
	Projects []Project `json:"projects"`
}

type Agent struct {
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
	Ready   bool   `json:"ready"`
	Why     string `json:"why,omitempty"`
	About   string `json:"about,omitempty"`
	Default bool   `json:"default,omitempty"`
	Busy    int    `json:"busy,omitempty"`
}

type AgentList struct {
	Agents []Agent `json:"agents"`
}

// Task is one unit of tracked work.
type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Goal      string    `json:"goal"`
	State     string    `json:"state"`
	Thread    string    `json:"thread,omitempty"`
	Transport string    `json:"transport,omitempty"`
	Project   string    `json:"project,omitempty"`
	Parent    string    `json:"parent,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Node      string    `json:"node,omitempty"`
	Origin    string    `json:"origin,omitempty"`
	Children  int       `json:"children"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TaskPage struct {
	Tasks      []Task `json:"tasks"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Event is one change on the stream. Cursor is also the SSE id: a client
// resumes after it with Last-Event-ID or the after parameter.
type Event struct {
	Cursor   string    `json:"cursor"`
	Type     EventType `json:"type"`
	At       time.Time `json:"at,omitzero"`
	Thread   string    `json:"thread,omitempty"`
	Turn     string    `json:"turn,omitempty"`
	Item     string    `json:"item,omitempty"`
	Task     string    `json:"task,omitempty"`
	Question string    `json:"question,omitempty"`
	State    string    `json:"state,omitempty"`
	Text     string    `json:"text,omitempty"`
	Silent   bool      `json:"silent,omitempty"`
	Progress *Progress `json:"progress,omitempty"`
	// SourceKind is the server's own name for the change.
	SourceKind string `json:"source_kind"`
}

// Progress is what a running turn is doing right now.
type Progress struct {
	Phase     string     `json:"phase,omitempty"`
	Agent     string     `json:"agent,omitempty"`
	Node      string     `json:"node,omitempty"`
	Model     string     `json:"model,omitempty"`
	Reasoning string     `json:"reasoning,omitempty"`
	Answer    string     `json:"answer,omitempty"`
	Tools     []ToolCall `json:"tools,omitempty"`
	Plan      []PlanLine `json:"plan,omitempty"`
}

type PlanLine struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// Error is the body of every v1 refusal except authentication's.
type Error struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

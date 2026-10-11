package turnapi

import (
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
	"time"
)

type Setup struct {
	Agent, Node, Harness, Model string
	// Mode is which identity the home lends this conversation.
	Mode string
	// Instructions is the assembled text a session is given, and Sections
	// is what it is made of, in order. The messaging server's guidance is
	// included only for a session that already holds a token for it. A
	// conversation without one is shown the rest, although the session its
	// next turn opens is also given that guidance when this hub runs the
	// messaging server and the agent speaks HTTP MCP.
	Instructions string
	Sections     []capability.Section
	MCPServers   []string
	// Applied says the current session already read these instructions.
	// When false the next turn will send them — a new session, or one
	// whose context changed.
	Applied bool
}

type RetainedChat struct {
	Abandoned         bool
	AbandonProjected  bool
	ForceStopLevel    string
	ForceStopRevision uint64
	AttemptID         string
	TaskID            string
	Conversation      string
	MessageID         string
	AgentID           string
	NodeID            string
	ProjectID         string
	Completed         bool
	// TaskState is the task's durable state when the execution was found:
	// a task set aside by the owner is not the execution's to carry on.
	TaskState task.State
}

type RelocationPlan struct {
	ID           string        `json:"id"`
	AttemptID    string        `json:"attempt_id"`
	TargetNodeID string        `json:"target_node_id"`
	Checkpoint   string        `json:"checkpoint"`
	CheckpointAt time.Time     `json:"checkpoint_at"`
	Automatic    bool          `json:"automatic"`
	Approved     bool          `json:"approved,omitempty"`
	Question     view.Question `json:"question"`
}

type TaskResume struct {
	Admission      task.ResumeAdmission
	Transport      string
	TaskID         string
	Goal           string
	Member         string
	ConversationID string
	ChatID         string
	MessageID      string
	Requester      string
	ChatType       string
}

type TaskNotice struct {
	Transport string
	TaskID    string
	ChatID    string
	MessageID string
	Requester string
	// Conversation is the thread the task lives in: on the console, the
	// notice belongs there, not to whichever thread is open.
	Conversation string
	Text         string
}

type Context struct {
	Conversation string
	Project      *ContextProject
	Agent        *AgentChoice
	Agents       []AgentChoice
}

type ContextProject struct {
	ID      string
	Node    string
	Path    string
	Level   string
	Repo    string
	Version int64
	// Bound is false when the conversation has no binding yet and the
	// project shown is the one its first line will bind.
	Bound bool
}

type Suggestion struct {
	Label  string
	Args   string
	Detail string
	Insert string
	Muted  bool
}

type AgentChoice struct {
	ID      string
	Node    string
	Harness string
	Model   string
	Ready   bool
	Why     string
	Usable  bool
	Because string
	Current bool
	// Place is where the agent would work in the current project; nil
	// when its machine has no workspace of it.
	Place *Placement
}

type Placement struct {
	Workspace string
	Kind      string
	Node      string
}

type Verb struct {
	Command string
	Args    string
	Summary string
}

type RetainedPlan struct {
	Abandoned                                      bool
	AbandonProjected                               bool
	ForceStopLevel                                 string
	ForceStopRevision                              uint64
	TaskID, Conversation, MessageID, PlanID, RunID string
	AttemptID, AgentID, NodeID, ProjectID          string
	Completed                                      bool
	// TaskState is the plan task's durable state when the plan was found.
	TaskState task.State
}

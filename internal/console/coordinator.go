package console

import (
	"context"

	"github.com/gopact-ai/steve/internal/turn"
)

// Coordinator is the turn coordinator as the console uses it: it runs a
// conversation's turns and knows where the conversation stands, what the
// console can be told and how a line parses.
type Coordinator interface {
	// Handle answers a line sent to a conversation.
	Handle(ctx context.Context, req turn.Request) (turn.Result, error)
	// Context is a conversation's project and agents.
	Context(ctx context.Context, conversationID string) (turn.Context, error)
	// SessionSetup is what an agent has in hand for a conversation.
	SessionSetup(ctx context.Context, conversationID, agentID string) (turn.Setup, error)
	// Suggest completes the line being typed.
	Suggest(ctx context.Context, conversationID, line string) []turn.Suggestion
	// VerbsFor names the verbs the console can be told, in the language of
	// the request.
	VerbsFor(ctx context.Context) []turn.Verb
	// ParseInput splits a line into its addressed target and parsed input.
	ParseInput(line string) (string, turn.ParsedInput)
	// InitializeConversation gives a conversation its first project
	// binding, on behalf of requester. It refuses a project that is unknown
	// or that requester cannot read, and a conversation already bound to
	// another project; binding again to the same project changes nothing.
	InitializeConversation(ctx context.Context, conversation, project, requester string) error
	// ResetConversationSessions forgets the agent sessions a conversation
	// holds, so its next turn starts a fresh one. A thread rewound to an
	// edited message must not be answered by a session that still
	// remembers the turns the transcript no longer has.
	ResetConversationSessions(ctx context.Context, conversationID string) error
}

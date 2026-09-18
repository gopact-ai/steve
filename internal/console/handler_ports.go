package console

import (
	"context"

	"github.com/gopact-ai/steve/internal/turn"
)

// The console only requires Handle of its Handler. The coordinator also
// knows where a conversation stands, what it can be told and how a line
// parses; a handler that does is asked, one that does not — a plain
// Handler in tests, or a channel without a coordinator — gets the
// console's own fallback. Each capability is its own port so a handler
// may offer any subset.

// contextProvider knows a conversation's project and agents.
type contextProvider interface {
	Context(ctx context.Context, conversationID string) (turn.Context, error)
}

// setupProvider answers what an agent has in hand for a conversation.
type setupProvider interface {
	SessionSetup(ctx context.Context, conversationID, agentID string) (turn.Setup, error)
}

// suggester completes the line being typed.
type suggester interface {
	Suggest(ctx context.Context, conversationID, line string) []turn.Suggestion
}

// verbLister names the verbs a conversation can be told.
type verbLister interface {
	Verbs() []turn.Verb
}

// localizedVerbLister names the verbs in the language of the request.
type localizedVerbLister interface {
	VerbsFor(context.Context) []turn.Verb
}

// inputParser splits a line into its addressed target and parsed input
// by the coordinator's rules rather than the console's default ones.
type inputParser interface {
	ParseInput(string) (string, turn.ParsedInput)
}

// sessionResetter forgets the agent sessions a conversation holds, so its
// next turn starts a fresh one. A thread rewound to an edited message
// must not be answered by a session that still remembers the turns the
// transcript no longer has.
type sessionResetter interface {
	ResetConversationSessions(ctx context.Context, conversationID string) error
}

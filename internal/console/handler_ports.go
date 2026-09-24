package console

import (
	"context"

	"github.com/gopact-ai/steve/internal/turn"
)

// verbLister names the verbs a conversation can be told.
type verbLister interface {
	Verbs() []turn.Verb
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

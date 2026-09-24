package gateway

import (
	"context"

	"github.com/gopact-ai/steve/internal/turn"
)

// Processor is the turn coordinator as the gateway uses it: it runs a
// conversation's turns, parses a line the way a turn would, and checks a
// scheduled run before the gateway announces it.
type Processor interface {
	// Handle answers a message sent to a conversation.
	Handle(ctx context.Context, req turn.Request) (turn.Result, error)
	// ParseInput splits a line into its addressed target and parsed
	// input. It selects the agent as Handle does, so a control addressed
	// by an alias or with no space before the command is still a
	// control, and it changes no conversation.
	ParseInput(line string) (string, turn.ParsedInput)
	// ValidateScheduled refuses a scheduled run unless its project and
	// requester are recorded, the conversation is still bound to that
	// project and the requester may write to it. It does not rebind the
	// conversation.
	ValidateScheduled(ctx context.Context, conversation, expectedProject, requester string) error
}

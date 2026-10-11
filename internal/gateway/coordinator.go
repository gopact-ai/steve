package gateway

import (
	"context"

	turn "github.com/gopact-ai/steve/internal/turnapi"
)

// Coordinator is the turn coordinator as the gateway uses it: it runs a
// conversation's turns, parses a line the way a turn would, and checks a
// scheduled run before the gateway announces it.
type Coordinator interface {
	turn.InputExecutor
	// ValidateScheduled refuses a scheduled run unless its project and
	// requester are recorded, the conversation is still bound to that
	// project and the requester may write to it. It does not rebind the
	// conversation.
	ValidateScheduled(ctx context.Context, conversation, expectedProject, requester string) error
}

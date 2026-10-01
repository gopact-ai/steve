package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// AbortRetainedSession cleans up a previously abandoned execution, without
// observing or resuming it. A command receipt alone never proves its exit.
func (m *Manager) AbortRetainedSession(ctx context.Context, at Placement, id, workdir string) (nodewire.SessionState, error) {
	return nodewire.SessionState{}, ErrStopUnconfirmed
}

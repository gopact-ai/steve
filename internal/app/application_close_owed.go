package app

import (
	"context"
	"sync"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type applicationSessionCloser interface {
	CloseSession(context.Context, harness.Placement, string) error
}

// applicationOwedCloses sends the closes a conversation let go of while the
// node running the session could not be reached.
type applicationOwedCloses struct {
	mu       sync.Mutex
	store    *state.Store
	attempts *attempt.Service
	tasks    *task.Store
	sessions applicationSessionCloser
}

func newApplicationOwedCloses(store *state.Store, attempts *attempt.Service, tasks *task.Store, sessions applicationSessionCloser) *applicationOwedCloses {
	return &applicationOwedCloses{store: store, attempts: attempts, tasks: tasks, sessions: sessions}
}

// Reconcile sends each close still owed.
func (c *applicationOwedCloses) Reconcile(context.Context) error {
	return nil
}

package turn

import (
	"context"
	"errors"
)

// sessionAdmissionState holds the jointly checked conversation fences and
// active turn admissions. Both maps use the original coordinator mutex.
type sessionAdmissionState struct {
	cancels  map[string]*turnEntry
	retiring map[string]bool
}

// idleCloseAdmission binds automatic closure to the original shared owner,
// without acquiring authority from a request-local coordinator view.
type idleCloseAdmission struct{ owner *coordinatorState }

// IdleCloseReservation supplies the automatic sweep's admission port. Every
// invocation checks current maintenance and conversation fences; no outcome
// is cached. Its release must run outside the caller's task/ledger locks.
func IdleCloseReservation(c *Coordinator) func(context.Context, string) (func(), error) {
	return (idleCloseAdmission{owner: c.coordinatorState}).reserve
}

func (a idleCloseAdmission) reserve(ctx context.Context, conversationID string) (func(), error) {
	owner := a.owner
	owner.requestMu.RLock()
	if owner.maintaining {
		owner.requestMu.RUnlock()
		return nil, errors.New("coordinator is under maintenance")
	}
	release, err := owner.beginConversationRetirement(ctx, conversationID)
	if err != nil {
		owner.requestMu.RUnlock()
		return nil, err
	}
	return func() { release(); owner.requestMu.RUnlock() }, nil
}

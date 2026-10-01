package state

import (
	"context"
	"errors"
)

// ProjectAbandonedSession retires only the named native context. It can be
// repeated after a crash without archiving a newer session in the same slot.
func (s *Store) ProjectAbandonedSession(ctx context.Context, conversation, agent string, owed OwedClose, closeNeeded bool) error {
	return errors.New("abandoned session projection is not available")
}

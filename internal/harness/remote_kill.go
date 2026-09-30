package harness

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/nodewire"
	"time"
)

// RetainedKiller forces the original native process to stop. Each call
// obtains a fresh receipt instead of reusing an earlier graceful stop.
type RetainedKiller interface {
	KillRetained(context.Context) (nodewire.SessionState, error)
}

func (s *managedSession) KillRetained(parent context.Context) (nodewire.SessionState, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	request := s.request(ctx, nodewire.SessionActionKill)
	state, err := s.call(ctx, request)
	if err != nil {
		return state, err
	}
	if !stopReceiptMatches(state, request) || !state.ProcessStopped {
		return state, errors.Join(ErrStopUnconfirmed, errors.New("native kill receipt does not prove the original process stopped"))
	}
	s.mu.Lock()
	s.stopState = state
	s.mu.Unlock()
	return state, nil
}

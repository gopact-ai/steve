package console

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type forceStopDriver interface {
	ForceStopAttempt(context.Context, string, string) error
}

var _ consoleapi.ForceStops = (*Service)(nil)

func (s *Service) SetForceStops(control forceStopDriver) { s.forceStops = control }

func (s *Service) ForceStop(ctx context.Context, id string) error {
	driver := s.forceStops
	if driver == nil {
		return errors.New("force stop is unavailable")
	}
	if s.owner == "" {
		return errors.New("force stop requires the owner")
	}
	return driver.ForceStopAttempt(ctx, id, s.owner)
}
func (w *recoveryStopWait) forceStop() error {
	w.s.mu.Lock()
	driver := w.s.recoveryDriver
	original := copyExchange(w.e.Exchange)
	owner := w.s.owner
	w.s.mu.Unlock()
	if w.requester == "" || w.requester != owner {
		return errors.New("force stop requires the owner")
	}
	retained, found, err := w.s.findRetained(w.ctx, driver, original)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("original execution is unavailable")
	}
	return w.s.ForceStop(w.ctx, retained.AttemptID)
}

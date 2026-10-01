package console

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
)

type forceStopDriver interface {
	ForceStopAttempt(context.Context, string, string, uint64) error
}

var _ consoleapi.ForceStops = (*Service)(nil)

func (s *Service) SetForceStops(control forceStopDriver) { s.forceStops = control }

func (s *Service) ForceStop(ctx context.Context, id string, expectedRevision uint64) error {
	driver := s.forceStops
	if driver == nil {
		return errors.New("force stop is unavailable")
	}
	if s.owner == "" {
		return errors.New("force stop requires the owner")
	}
	return driver.ForceStopAttempt(ctx, id, s.owner, expectedRevision)
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
	text := i18n.New(i18n.FromLang(w.base.Locale))
	base := w.base
	base.TaskID, base.AttemptID = retained.TaskID, retained.AttemptID
	answer, err := w.s.askUser(w.ctx, base, view.Question{
		Kind: "recovery", Required: true,
		Title:   text.T(i18n.ConsoleForceConfirmTitle, retained.TaskID),
		Message: text.T(i18n.ConsoleForceConfirmBody, retained.TaskID, retained.AttemptID),
		Choices: []view.Choice{
			{Value: "confirm-force-stop", Label: text.T(i18n.ConsoleForceConfirm)},
			{Value: "cancel-force-stop", Label: text.T(i18n.ConsoleForceCancel)},
		},
	}, false)
	if err != nil {
		return err
	}
	if answer.Value != "confirm-force-stop" {
		return nil
	}
	return w.s.ForceStop(w.ctx, retained.AttemptID, retained.ForceStopRevision)
}

package console

import (
	"errors"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
)

func (w *recoveryStopWait) abandon() error {
	w.s.mu.Lock()
	driver, owner := w.s.recoveryDriver, w.s.owner
	original := copyExchange(w.e.Exchange)
	w.s.mu.Unlock()
	if w.requester == "" || w.requester != owner {
		return errors.New("abandonment requires the owner")
	}
	retained, found, err := w.s.findRetained(w.ctx, driver, original)
	if err != nil {
		return err
	}
	if !found || retained.Abandoned || retained.ForceStopLevel != "exhausted" {
		return errors.New("original force stop is not exhausted")
	}
	text := i18n.New(i18n.FromLang(w.base.Locale))
	base := w.base
	base.TaskID, base.AttemptID = retained.TaskID, retained.AttemptID
	answer, err := w.s.askUser(w.ctx, base, view.Question{Kind: "recovery", Required: true, Title: text.T(i18n.ConsoleAbandonTitle, retained.TaskID), Message: text.T(i18n.ConsoleAbandonBody, retained.AttemptID), Choices: []view.Choice{{Value: "confirm-abandon", Label: text.T(i18n.ConsoleAbandonConfirm)}, {Value: "cancel-abandon", Label: text.T(i18n.ConsoleForceCancel)}}}, false)
	if err != nil {
		return err
	}
	if answer.Value != "confirm-abandon" {
		return nil
	}
	_, err = w.s.Abandon(w.ctx, retained.AttemptID, retained.ForceStopRevision)
	return err
}

func (w *recoveryStopWait) canAbandon() bool {
	w.s.mu.Lock()
	driver := w.s.recoveryDriver
	original := copyExchange(w.e.Exchange)
	enabled := w.s.abandons != nil
	w.s.mu.Unlock()
	if !enabled || driver == nil {
		return false
	}
	retained, found, err := w.s.findRetained(w.ctx, driver, original)
	return err == nil && found && !retained.Abandoned && retained.ForceStopLevel == "exhausted"
}

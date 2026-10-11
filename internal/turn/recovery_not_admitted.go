package turn

import (
	"context"
	"errors"
	"strings"
)

// ConfirmNeverAdmitted proves that one console input ended before any
// execution was admitted. It is read-only and grants neither execution nor
// permission to replay. Old unlabelled accounting requires explicit task
// cancellation and fully reconciled historical executions. Missing records
// and an empty retained-session projection cannot establish this fact.
func (c *Coordinator) ConfirmNeverAdmitted(ctx context.Context, req Request) (bool, error) {
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	if c.maintaining {
		return false, errors.New("never-admitted recovery proof is unavailable")
	}
	if req.Source.Channel != "console" || !strings.HasPrefix(req.Source.ConversationID, consolePrefix) ||
		req.Admission.ExchangeID == "" || req.Source.MessageID != "web-"+req.Admission.ExchangeID ||
		c.ownerOpenID == "" || req.Actor.ID != c.ownerOpenID {
		return false, errors.New("never-admitted proof requires the original console input and owner")
	}
	// Keep the live turn-slot exclusion across the committed read. This is
	// only an additional guard; absence of a slot is never the proof itself.
	c.mu.Lock()
	defer c.mu.Unlock()
	tracked, confirmed, err := c.attempts.UnadmittedTurn(ctx, req.Address())
	if err != nil || !confirmed {
		return false, err
	}
	if tracked.Transport != req.Source.Channel || tracked.Channel != req.Source.ConversationID || tracked.Requester != req.Actor.ID ||
		tracked.Origin != req.Source.Origin || (req.Admission.ExpectedTask != "" && tracked.ID != req.Admission.ExpectedTask) ||
		(req.Admission.ExpectedProject != "" && tracked.ProjectID != req.Admission.ExpectedProject) {
		return false, errors.New("never-admitted accounting belongs to another input owner")
	}
	if c.cancels[sessionKey(req.Source.ConversationID, tracked.Member)] != nil {
		return false, nil
	}
	return true, nil
}

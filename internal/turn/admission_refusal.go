package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
)

func (c *Coordinator) openAttemptRefusal(req Request, taskID string) string {
	text := c.text.T(i18n.TaskOpenAttempt, taskID, protocol.CommandTasks)
	if req.Channel == "console" {
		text += " " + c.text.T(i18n.TurnRefusalConsoleRecheck)
	}
	return text
}

// The blocked writer need not belong to this conversation, or even this
// channel. Name its task before offering actions scoped to its owner.
func (c *Coordinator) unconfirmedWriterRefusal(ctx context.Context, req Request, cause error) UserError {
	refusal := UserError{Text: c.text.T(i18n.TurnWriterUnconfirmed), Cause: cause}
	var stopped attempt.StopUnconfirmed
	if !errors.As(cause, &stopped) {
		return refusal
	}
	record, err := c.attempts.Get(ctx, stopped.Holder)
	if err != nil {
		return refusal
	}
	holder, ok := c.tasks.Get(record.TaskID)
	if !ok {
		return refusal
	}
	switch {
	case holder.Channel == req.ConversationID && holder.Transport == req.Channel:
		refusal.Text = c.text.T(i18n.TurnWriterTask, holder.ID, protocol.CommandTasks)
	case holder.Transport == "console" && req.Channel == "console":
		refusal.Text = c.text.T(i18n.TurnWriterConsoleTask, holder.ID, holder.Member, holder.Channel)
	case holder.Transport == "feishu" && req.Channel == "feishu":
		refusal.Text = c.text.T(i18n.TurnWriterChatTask, holder.ID, holder.Member, holder.Channel, protocol.CommandTasks)
	default:
		refusal.Text = c.text.T(i18n.TurnWriterOtherTask, holder.ID, holder.Member, holder.Channel, holder.Transport)
	}
	if req.Channel == "console" && holder.Transport == "console" {
		refusal.Text += " " + c.text.T(i18n.TurnRefusalConsoleRecheck)
	}
	return refusal
}

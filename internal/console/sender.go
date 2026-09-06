package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// MessageSender delivers agent milestones to their exact console conversation.
// Inbound exchange anchors and outbound reply receipts are distinct identities.
type MessageSender struct {
	Console *Service
}

var _ channel.Messenger = MessageSender{}

func (s MessageSender) validate(ctx context.Context, address channel.Address) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Console == nil {
		return errors.New("console messaging is unavailable")
	}
	if address.Channel != "console" || !strings.HasPrefix(address.Conversation, Prefix) || address.Conversation == Prefix || address.Message == "" {
		return errors.New("console messaging requires a console channel, conversation and message")
	}
	return nil
}

func consoleMessage(message channel.Message) (channel.Message, error) {
	if message.Format == "" {
		message.Format = "markdown"
	}
	if message.Format != "markdown" && message.Format != "text" {
		return message, fmt.Errorf("unsupported console message format %q", message.Format)
	}
	return message, nil
}

func (s MessageSender) Send(ctx context.Context, address channel.Address, message channel.Message) (string, error) {
	if err := s.validate(ctx, address); err != nil {
		return "", err
	}
	message, err := consoleMessage(message)
	if err != nil {
		return "", err
	}
	c := s.Console
	c.mu.Lock()
	defer c.mu.Unlock()
	var exchangeID string
	for _, exchange := range c.exchanges[address.Conversation] {
		if address.Message == AnchorMark+exchange.ID {
			exchangeID = exchange.ID
			break
		}
	}
	if exchangeID == "" {
		return "", errors.New("console anchor does not belong to this conversation")
	}
	before := c.replies[address.Conversation]
	r := consoleapi.Reply{ID: newReplyID(), ExchangeID: exchangeID, At: time.Now().UTC(), Conversation: address.Conversation,
		Text: message.Content, Format: message.Format, Title: message.Attribution, Kind: "milestone"}
	r = c.recordLocked(r)
	if err := c.save(); err != nil {
		c.replies[address.Conversation] = before
		return "", err
	}
	c.publishReply(r)
	return r.ID, nil
}

func (s MessageSender) Update(ctx context.Context, address channel.Address, message channel.Message) error {
	if err := s.validate(ctx, address); err != nil {
		return err
	}
	message, err := consoleMessage(message)
	if err != nil {
		return err
	}
	c := s.Console
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, r := range c.replies[address.Conversation] {
		if r.ID != address.Message || r.Kind != "milestone" {
			continue
		}
		updated := r
		updated.Text, updated.Format, updated.Title = message.Content, message.Format, message.Attribution
		updated.Revision = replyRevision(updated)
		c.replies[address.Conversation][i] = updated
		if err := c.save(); err != nil {
			c.replies[address.Conversation][i] = r
			return err
		}
		c.publishReply(updated)
		return nil
	}
	return errors.New("console milestone does not belong to this conversation")
}

func (s MessageSender) Recall(ctx context.Context, address channel.Address) error {
	if err := s.validate(ctx, address); err != nil {
		return err
	}
	c := s.Console
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.replies[address.Conversation]
	for i, r := range list {
		if r.ID != address.Message || r.Kind != "milestone" {
			continue
		}
		c.replies[address.Conversation] = append(list[:i:i], list[i+1:]...)
		if err := c.save(); err != nil {
			c.replies[address.Conversation] = list
			return err
		}
		if c.model != nil {
			c.model.Publish(readmodel.Event{At: time.Now().UTC(), Kind: "console.recalled", Conversation: address.Conversation, ReplyID: r.ID})
		}
		return nil
	}
	return errors.New("console milestone does not belong to this conversation")
}

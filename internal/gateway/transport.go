package gateway

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/ledger"
	"time"
)

func (g *Gateway) Name() string { return g.name }

func (g *Gateway) validateScheduled(ctx context.Context, conversation, project, actor string) error {

	return g.coordinator.ValidateScheduled(ctx, conversation, project, actor)
}

// Empty is the historical Feishu wire image. Keeping it avoids changing the
// immutable payload of already accepted messages on an upgrade/retry.
func (g *Gateway) inputTransport() string {
	if g.name == "feishu" {
		return ""
	}
	return g.name
}
func inputChannel(input gatewayInput) string {
	if input.Transport == "" {
		return "feishu"
	}
	return input.Transport
}

// BindAdapter lets a plain native transport share the complete admission,
// recovery and text delivery path without implementing Feishu cards/reactions.
func (g *Gateway) BindAdapter(adapter channel.Adapter) {
	if rich, ok := adapter.(channel.ConversationTransport); ok {
		g.BindChannel(rich)
		return
	}
	g.BindChannel(plainAdapter{Adapter: adapter, gateway: g})
}

type plainAdapter struct {
	channel.Adapter
	gateway *Gateway
}

func (p plainAdapter) address(ctx context.Context, id string) (channel.Address, error) {
	idConversation, err := conversation.MessageConversation(ctx, p.gateway.recoveryLedger, p.gateway.name, id)
	return channel.Address{Channel: p.gateway.name, Conversation: idConversation, Message: id}, err
}
func (p plainAdapter) Reply(ctx context.Context, id, text string) error {
	_, err := p.ReplyText(ctx, id, text)
	return err
}
func (p plainAdapter) ReplyText(ctx context.Context, id, text string) (string, error) {
	a, err := p.address(ctx, id)
	if err != nil {
		return "", err
	}
	receipt, err := p.Send(ctx, a, channel.Message{Content: text, Format: "markdown"})
	if err != nil {
		return receipt, err
	}
	if receipt == "" {
		return "", channel.ErrOutcomeUnknown
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err = p.gateway.recoveryLedger.Update(commit, func(tx *ledger.Tx) error { return conversation.BindMessageTx(tx, a.Channel, receipt, a.Conversation) })
	if err != nil {
		return receipt, errors.Join(channel.ErrOutcomeUnknown, err)
	}
	return receipt, nil
}
func (p plainAdapter) ReplyCard(context.Context, string, []byte) (string, error) {
	return "", channel.ErrUnsupported
}
func (p plainAdapter) PatchCard(context.Context, string, []byte) error { return channel.ErrUnsupported }
func (p plainAdapter) ReplyThread(context.Context, string, string) (string, string, error) {
	return "", "", channel.ErrUnsupported
}
func (p plainAdapter) AddReaction(context.Context, string, string) (string, error) {
	return "", channel.ErrUnsupported
}
func (p plainAdapter) RemoveReaction(context.Context, string, string) error {
	return channel.ErrUnsupported
}
func (p plainAdapter) DeleteMessage(ctx context.Context, id string) error {
	a, err := p.address(ctx, id)
	if err != nil {
		return err
	}
	return p.Recall(ctx, a)
}

package app

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
)

// textOnlyGatewayChannel implements the gateway channel with successful
// no-ops and refuses every card and topic thread, so final answers take the
// receipted text fallback and a /t seed fails. Fakes embed it and override
// the calls they observe.
type textOnlyGatewayChannel struct{}

var _ gateway.Channel = textOnlyGatewayChannel{}

func (textOnlyGatewayChannel) Reply(context.Context, string, string) error { return nil }
func (textOnlyGatewayChannel) ReplyText(context.Context, string, string) (string, error) {
	return "", nil
}
func (textOnlyGatewayChannel) ReplyCard(context.Context, string, []byte) (string, error) {
	return "", errors.New("card refused")
}
func (textOnlyGatewayChannel) PatchCard(context.Context, string, []byte) error { return nil }
func (textOnlyGatewayChannel) ReplyThread(context.Context, string, string) (string, string, error) {
	return "", "", errors.New("topic refused")
}
func (textOnlyGatewayChannel) EnrichInput(_ context.Context, msg feishu.InboundMessage) feishu.InboundMessage {
	return msg
}
func (textOnlyGatewayChannel) AddReaction(context.Context, string, string) (string, error) {
	return "", nil
}
func (textOnlyGatewayChannel) RemoveReaction(context.Context, string, string) error { return nil }
func (textOnlyGatewayChannel) DeleteMessage(context.Context, string) error          { return nil }

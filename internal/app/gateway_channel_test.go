package app

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/channel/feishu"
)

// textOnlyGatewayChannel implements the gateway channel with successful
// no-ops and refuses every card, so final answers take the receipted text
// fallback. Fakes embed it and override the calls they observe.
type textOnlyGatewayChannel struct{}

func (textOnlyGatewayChannel) Reply(context.Context, string, string) error { return nil }
func (textOnlyGatewayChannel) ReplyText(context.Context, string, string) (string, error) {
	return "", nil
}
func (textOnlyGatewayChannel) ReplyCard(context.Context, string, []byte) (string, error) {
	return "", errors.New("card refused")
}
func (textOnlyGatewayChannel) PatchCard(context.Context, string, []byte) error { return nil }
func (textOnlyGatewayChannel) ReplyThread(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (textOnlyGatewayChannel) EnrichInput(_ context.Context, msg feishu.InboundMessage) feishu.InboundMessage {
	return msg
}
func (textOnlyGatewayChannel) AddReaction(context.Context, string, string) (string, error) {
	return "", nil
}
func (textOnlyGatewayChannel) RemoveReaction(context.Context, string, string) error { return nil }
func (textOnlyGatewayChannel) DeleteMessage(context.Context, string) error          { return nil }

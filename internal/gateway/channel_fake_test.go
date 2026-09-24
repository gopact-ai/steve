package gateway

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/channel/feishu"
)

var (
	errCardRefused  = errors.New("card refused")
	errTopicRefused = errors.New("topic refused")
)

// nopChannel refuses every card and topic thread definitively, so a final
// answer takes the receipted text fallback and a /t seed fails. Every other
// call succeeds without a message id, and receipted text without an id is an
// unknown outcome. Fakes embed it and override only the calls they observe.
type nopChannel struct{}

var _ Channel = nopChannel{}

func (nopChannel) Reply(context.Context, string, string) error               { return nil }
func (nopChannel) ReplyText(context.Context, string, string) (string, error) { return "", nil }
func (nopChannel) ReplyCard(context.Context, string, []byte) (string, error) {
	return "", errCardRefused
}
func (nopChannel) PatchCard(context.Context, string, []byte) error { return nil }
func (nopChannel) ReplyThread(context.Context, string, string) (string, string, error) {
	return "", "", errTopicRefused
}
func (nopChannel) EnrichInput(_ context.Context, msg feishu.InboundMessage) feishu.InboundMessage {
	return msg
}
func (nopChannel) AddReaction(context.Context, string, string) (string, error) { return "", nil }
func (nopChannel) RemoveReaction(context.Context, string, string) error        { return nil }
func (nopChannel) DeleteMessage(context.Context, string) error                 { return nil }

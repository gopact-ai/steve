package gateway

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/channel/feishu"
)

// nopChannel implements every channel method as a successful no-op without
// receipts. Fakes embed it and override only the calls they observe.
type nopChannel struct{}

var _ Channel = nopChannel{}

func (nopChannel) Reply(context.Context, string, string) error               { return nil }
func (nopChannel) ReplyText(context.Context, string, string) (string, error) { return "", nil }
func (nopChannel) ReplyCard(context.Context, string, []byte) (string, error) { return "", nil }
func (nopChannel) PatchCard(context.Context, string, []byte) error           { return nil }
func (nopChannel) ReplyThread(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (nopChannel) EnrichInput(_ context.Context, msg feishu.InboundMessage) feishu.InboundMessage {
	return msg
}
func (nopChannel) AddReaction(context.Context, string, string) (string, error) { return "", nil }
func (nopChannel) RemoveReaction(context.Context, string, string) error        { return nil }
func (nopChannel) DeleteMessage(context.Context, string) error                 { return nil }

var errCardRefused = errors.New("card refused")

// textOnlyChannel refuses every card definitively, so a final answer takes
// the receipted text fallback.
type textOnlyChannel struct{ nopChannel }

func (textOnlyChannel) ReplyCard(context.Context, string, []byte) (string, error) {
	return "", errCardRefused
}

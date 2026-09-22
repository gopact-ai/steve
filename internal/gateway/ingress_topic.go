package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
)

type topicReceipt struct {
	InputID string `json:"input_id"`
	Anchor  string `json:"anchor"`
	Thread  string `json:"thread"`
}

func topicTask(msg feishu.InboundMessage) (string, bool) {
	cmd, rest := protocol.ParseCommand(strings.TrimSpace(msg.Text))
	return rest, cmd == protocol.CommandTopic && !silentListen(msg)
}

func (g *Gateway) topicGuard(msg feishu.InboundMessage) string {
	text, topic := topicTask(msg)
	if !topic {
		return ""
	}
	if strings.TrimSpace(text) == "" {
		return g.text.T(i18n.TopicNeedsTask, protocol.CommandTopic, protocol.CommandTopic)
	}
	if conversationID(msg) != msg.ChatID {
		return g.text.T(i18n.TopicAlready)
	}
	if _, ok := g.ch.(topicSeeder); !ok {
		return g.text.T(i18n.TopicFailed)
	}
	return ""
}

func topicMessage(input gatewayInput, receipt ledger.CommandRecord, key string) (feishu.InboundMessage, error) {
	msg := input.Message
	text, topic := topicTask(msg)
	var proof topicReceipt
	if !topic || text == "" || conversationID(msg) != msg.ChatID ||
		receipt.ID != key+"/topic" || receipt.Kind != "gateway-input-topic" || receipt.Actor != msg.SenderOpenID ||
		receipt.FinishedAt == nil || receipt.Error != "" || json.Unmarshal(receipt.Result, &proof) != nil ||
		proof.InputID != key || proof.Anchor == "" || proof.Thread == "" || proof.Thread == msg.ChatID {
		return msg, fmt.Errorf("%w: topic has no successful route receipt", channel.ErrOutcomeUnknown)
	}
	msg.MessageID, msg.ConversationID, msg.Text, msg.Quote = proof.Anchor, proof.Thread, text, ""
	return msg, nil
}

// prepareTopic derives a new address from one input's external seed receipt.
// Unknown seed effects stay fenced, including a missing response after success.
func (g *Gateway) prepareTopic(ctx context.Context, book *ledger.Ledger, key string, input gatewayInput) (feishu.InboundMessage, error) {
	text, topic := topicTask(input.Message)
	if !topic {
		return input.Message, nil
	}
	receipt, found, err := book.CommandReceipt(ctx, key+"/topic")
	if err != nil {
		return input.Message, err
	}
	if found {
		return topicMessage(input, receipt, key)
	}
	seeder, ok := g.ch.(topicSeeder)
	if !ok {
		return input.Message, errors.New("gateway topic seeder unavailable")
	}
	_, _, err = book.Command(ctx, key+"/topic", "gateway-input-topic", input.Message.SenderOpenID, func(ctx context.Context) (json.RawMessage, error) {
		anchor, thread, err := seeder.ReplyThread(ctx, input.Message.MessageID, text)
		if err != nil {
			return nil, noticeError(err)
		}
		if anchor == "" || thread == "" || thread == input.Message.ChatID {
			return nil, channel.ErrOutcomeUnknown
		}
		return json.Marshal(topicReceipt{InputID: key, Anchor: anchor, Thread: thread})
	})
	if err != nil {
		return input.Message, fmt.Errorf("topic seed requires reconciliation: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	receipt, _, err = book.CommandReceipt(ctx, key+"/topic")
	if err != nil {
		return input.Message, err
	}
	return topicMessage(input, receipt, key)
}

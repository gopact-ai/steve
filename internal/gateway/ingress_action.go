package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/channel"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
)

// The rendered card already carries the stable request identity. Encoding
// the tuple is reversible: this is not a fresh click ID or a payload hash.
func actionInputKey(action channel.Action) string {
	raw, _ := json.Marshal([]string{action.RequestID, action.Action, action.MessageID})
	return "gateway-action/" + base64.RawURLEncoding.EncodeToString(raw)
}

func (g *Gateway) handleDurableAction(action channel.Action) channel.Feedback {
	fail := func() channel.Feedback {
		return channel.Feedback{Type: "error", Content: g.text.T(i18n.ApprovalDenied)}
	}
	if action.RequestID == "" || action.MessageID == "" || action.ActorID == "" || action.ChatID == "" || g.ingressContext == nil {
		return fail()
	}
	key := actionInputKey(action)
	if g.name != "feishu" {
		key = g.name + "/" + key
	}
	input, found, err := g.acceptedAction(key, action)
	if err != nil {
		return fail()
	}
	if !found {
		var expired bool
		input, expired, err = g.actionInput(action)
		if err != nil {
			return fail()
		}
		if expired {
			return channel.Feedback{Type: "info", Content: g.text.T(i18n.TurnActionExpired)}
		}
	}
	if err := g.acceptAndWake(key, input); err != nil {
		return fail()
	}
	if action.Action == "turn_retry" {
		g.mu.Lock()
		delete(g.turns, action.RequestID)
		g.mu.Unlock()
		return channel.Feedback{Type: "success", Content: g.text.T(i18n.TurnRetryStarted)}
	}
	return channel.Feedback{Type: "success", Content: g.text.T(i18n.TurnStopRequested)}
}

func (g *Gateway) acceptedAction(key string, action channel.Action) (gatewayInput, bool, error) {
	record, found, err := g.recoveryLedger.CommandReceipt(g.ingressContext, key)
	if err != nil || !found {
		return gatewayInput{}, found, err
	}
	input, err := decodeGatewayInput(record)
	if err != nil {
		return input, true, err
	}
	if input.Action == nil || *input.Action != action || record.Actor != action.ActorID {
		return input, true, fmt.Errorf("%w: card action acceptance identity changed", ledger.ErrConflict)
	}
	return input, true, nil
}

func (g *Gateway) actionInput(action channel.Action) (gatewayInput, bool, error) {
	input := gatewayInput{Action: &action, Transport: g.inputTransport()}
	if action.Action == "history_restore" {
		input.Message = channel.Input{ChatID: action.ChatID, ConversationID: action.RequestID,
			MessageID: action.MessageID, ActorID: action.ActorID, Text: string(protocol.CommandHistory) + " 1", Mentioned: true}
		return input, false, nil
	}
	g.mu.Lock()
	entry := g.turns[action.RequestID]
	if entry == nil {
		g.mu.Unlock()
		return input, true, nil
	}
	current := *entry
	g.mu.Unlock()
	if current.msg.ActorID != action.ActorID || current.cardID != action.MessageID || current.msg.ChatID != action.ChatID {
		return input, false, errors.New("card action does not match its rendered owner")
	}
	if (action.Action == "turn_retry") == current.running {
		return input, true, nil
	}
	input.Message = current.msg
	if action.Action == "turn_cancel" {
		input.Message.Text = string(protocol.CommandCancel)
		input.Message.Images, input.Message.ImageKeys, input.Message.Quote = nil, nil, ""
	}
	return input, false, nil
}

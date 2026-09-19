package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

// SetIngressLifetime shares the application's close-before-join worker owner.
// Wire it before accepting channel callbacks.
func (g *Gateway) SetIngressLifetime(ctx context.Context, workers RecoveryWorkers) {
	g.ingressContext, g.ingressWorkers = ctx, workers
}

type inputParser interface {
	ParseInput(string) (string, turn.ParsedInput)
}

func (g *Gateway) immediateInput(text string) bool {
	if parser, ok := g.processor.(inputParser); ok {
		_, parsed := parser.ParseInput(text)
		return parsed.Interrupt || parsed.Control()
	}
	return turn.ImmediateInput(text)
}

func (g *Gateway) acceptAndWake(key string, input gatewayInput) error {
	ctx := g.ingressContext
	if ctx == nil {
		return errors.New("gateway ingress lifetime is not configured")
	}
	if err := g.acceptInput(ctx, key, input); err != nil {
		return err
	}
	receipt, found, err := g.recoveryLedger.CommandReceipt(ctx, key)
	if err != nil || !found {
		return fmt.Errorf("gateway accepted input cannot be read: %w", err)
	}
	driver, _ := g.processor.(RecoveryDriver)
	run, release, err := g.claimQueued(ctx, g.recoveryLedger, receipt, driver, nil, false)
	if errors.Is(err, channel.ErrDeliveryQueued) {
		return nil // Acceptance is durable; the running reconciler owns retry.
	}
	if err != nil {
		return err
	}
	if g.ingressWorkers == nil || !g.ingressWorkers.Go(func() {
		defer release()
		if err := run(); err != nil && ctx.Err() == nil {
			slog.Error("gateway accepted input remains pending", "input", key, "error", err)
		}
	}) {
		release()
		// This is not a lost input or a dispatch permission: shutdown leaves
		// the accepted command pending for startup, without a detached worker.
	}
	return nil
}

func pendingGatewayInputs(ctx context.Context, book *ledger.Ledger) ([]ledger.CommandRecord, error) {
	inputs, err := book.PendingCommands(ctx, recoveryInputKind)
	if err != nil {
		return nil, err
	}
	ordinary, err := book.PendingCommands(ctx, gatewayInputKind)
	if err != nil {
		return nil, err
	}
	inputs = append(inputs, ordinary...)
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].ReceivedAt.Equal(inputs[j].ReceivedAt) {
			return inputs[i].ID < inputs[j].ID
		}
		return inputs[i].ReceivedAt.Before(inputs[j].ReceivedAt)
	})
	return inputs, nil
}

func decodeGatewayInput(receipt ledger.CommandRecord) (gatewayInput, error) {
	var input gatewayInput
	if receipt.Kind != gatewayInputKind || receipt.FinishedAt == nil || receipt.Error != "" ||
		json.Unmarshal(receipt.Result, &input) != nil || input.Message.SenderOpenID != receipt.Actor ||
		input.Message.MessageID == "" || conversationID(input.Message) == "" {
		return input, errors.New("gateway input has invalid acceptance")
	}
	return input, nil
}

// claimQueued uses the same input and conversation owner for ingress, runtime
// recovery and manual wakes. Only explicit controls/interrupts may join an
// already serving conversation; normal input remains accepted without a
// premature dispatch reservation while its owner is busy. Topic preparation
// may share the original chat, but must acquire its own thread before Handle.
func (g *Gateway) claimQueued(ctx context.Context, book *ledger.Ledger, receipt ledger.CommandRecord, driver RecoveryDriver, revive func(string, string) error, wait bool) (func() error, func(), error) {
	if receipt.Kind != gatewayInputKind {
		release, err := g.claimRecovery(ctx, receipt, wait)
		return func() error { return g.recoverAcceptedInput(ctx, book, receipt, driver, revive) }, release, err
	}
	input, err := decodeGatewayInput(receipt)
	if err != nil {
		return nil, nil, err
	}
	conversation := conversationID(input.Message)
	_, seedPending := topicTask(input.Message)
	if seedPending {
		route, found, err := book.CommandReceipt(ctx, receipt.ID+"/topic")
		if err != nil {
			return nil, nil, err
		}
		if found && route.FinishedAt != nil && route.Error == "" {
			msg, err := topicMessage(input, route, receipt.ID)
			if err != nil {
				return nil, nil, err
			}
			conversation = conversationID(msg)
			seedPending = false
		}
	}
	claim, err := g.claimOrdinary(ctx, receipt.ID, conversation, wait, seedPending || g.immediateInput(input.Message.Text))
	if err != nil {
		return nil, nil, err
	}
	return func() error { return g.consumeInput(ctx, book, receipt.ID, input, driver, claim) }, claim.close, nil
}

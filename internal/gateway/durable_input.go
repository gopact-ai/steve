package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
)

func (g *Gateway) acceptInput(ctx context.Context, key string, input gatewayInput) error {
	if key == "" || input.Message.MessageID == "" || conversationID(input.Message) == "" {
		return errors.New("gateway durable input requires its original message and conversation")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return g.recoveryLedger.RecordCommand(ctx, key, gatewayInputKind, input.Message.SenderOpenID, raw)
}

func (g *Gateway) consumeInput(ctx context.Context, book *ledger.Ledger, key string, input gatewayInput, driver RecoveryDriver, claim *inputClaim) error {
	actor := input.Message.SenderOpenID
	ack := func() error { return acknowledgeInput(ctx, book, key, actor) }
	if err := ack(); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("delivery acknowledgement: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	msg := input.Message
	// Remote context reads are retryable, but never an input/dispatch proof.
	// Complete or unknown dispatches are observed without fetching it again.
	_, dispatched, err := book.CommandReceipt(ctx, key+"/dispatch")
	if err != nil {
		return err
	}
	if !dispatched && g.ch != nil {
		msg = g.ch.EnrichInput(ctx, msg)
	}
	input.Message = msg
	guard := g.topicGuard(msg)
	if guard == "" {
		var err error
		msg, err = g.prepareTopic(ctx, book, key, input)
		if err != nil {
			return err
		}
		if err := claim.move(ctx, conversationID(msg)); err != nil {
			return err
		}
	}
	output, ui, err := g.dispatchInput(ctx, book, key, input, msg, guard, driver)
	if ui != nil {
		defer ui.closeProgress()
	}
	if err != nil {
		return err
	}
	if err := g.deliverInput(ctx, book, key, msg, output, ui); err != nil {
		return err
	}
	return ack()
}

func (g *Gateway) dispatchInput(ctx context.Context, book *ledger.Ledger, key string, input gatewayInput, msg feishu.InboundMessage, guard string, driver RecoveryDriver) (recoveredOutput, *turnUI, error) {
	actor := input.Message.SenderOpenID
	request := g.taskRequest(msg, input.ExpectedTask, nil)
	var output recoveredOutput
	var ui *turnUI
	dispatch, found, err := book.CommandReceipt(ctx, key+"/dispatch")
	if err != nil {
		return output, ui, err
	}
	if found && (dispatch.Kind != "gateway-input-dispatch" || dispatch.Actor != actor) {
		return output, ui, fmt.Errorf("%w: input dispatch identity changed", ledger.ErrConflict)
	}
	if guard != "" {
		output.Result.Text = guard
		raw, err := json.Marshal(output)
		if err == nil {
			err = book.RecordCommand(ctx, key+"/dispatch", "gateway-input-dispatch", actor, raw)
		}
		return output, ui, err
	}
	raw, replayed, err := book.Command(ctx, key+"/dispatch", "gateway-input-dispatch", actor, func(ctx context.Context) (json.RawMessage, error) {
		if g.gate != nil && !g.scheduleControl(msg.Text) {
			g.gate.Anchor(conversationID(msg), channel.Address{Channel: "feishu", Conversation: conversationID(msg), Message: msg.MessageID})
		}
		if input.Action != nil && input.Action.Action == "turn_retry" {
			g.recall(input.Action.MessageID)
		}
		ui = g.newTurnUI(msg, silentListen(msg))
		request = g.taskRequest(msg, input.ExpectedTask, ui)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var admitted string
		var receiptErr error
		request.OnTurnReady = func(taskID, attemptID string) {
			admitted = attemptID
			receiptErr = rememberRecoveryAttempt(ctx, book, key, actor, request, taskID, attemptID)
			if receiptErr != nil {
				cancel()
			}
		}
		result, runErr := g.processor.Handle(ctx, request)
		runErr = errors.Join(runErr, receiptErr)
		output := recoveredResult(result, runErr)
		// An execution layer block arrives here only through turn's
		// planRecoveryError, stripped of its attempt and marked
		// ErrStopUnconfirmed (see agentexec.RecoveryBlocked).
		var blocked *agentexec.RecoveryBlocked
		output.Recover = receiptErr != nil || result.Attempt != admitted ||
			admitted != "" && (errors.As(runErr, &blocked) || errors.Is(runErr, harness.ErrStopUnconfirmed))
		return json.Marshal(output)
	})
	if err != nil && !replayed {
		return output, ui, err
	}
	if err != nil || json.Unmarshal(raw, &output) != nil || output.Recover {
		output, err = recoverDispatchedAttempt(ctx, book, key, actor, request, driver)
	}
	return output, ui, err
}

func (g *Gateway) deliverInput(ctx context.Context, book *ledger.Ledger, key string, msg feishu.InboundMessage, output recoveredOutput, ui *turnUI) error {
	// Listening policy can intentionally produce no external message. Record
	// that local disposition separately; it is never a fabricated /reply or
	// evidence that Feishu received a final message.
	if silentListen(msg) && (output.Error != "" || output.Result.Text == "") {
		raw, err := json.Marshal(ledger.CommandProof{CommandID: key, Receipt: "unmentioned-empty-output"})
		if err != nil {
			return err
		}
		return book.RecordCommand(ctx, key+"/suppressed", "gateway-input-suppressed", msg.SenderOpenID, raw)
	}
	runErr := output.runError()
	_, _, err := book.Command(ctx, key+"/reply", "gateway-input-reply", msg.SenderOpenID, func(context.Context) (json.RawMessage, error) {
		if ui == nil {
			ui = g.newResultUI(msg)
		}
		id, err := ui.finish(output.Result, runErr)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, channel.ErrOutcomeUnknown
		}
		return json.Marshal(ledger.CommandProof{CommandID: key, Receipt: id})
	})
	if err != nil {
		return fmt.Errorf("result delivery requires reconciliation: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	return nil
}

func acknowledgeInput(ctx context.Context, book *ledger.Ledger, key, actor string) error {
	_, replied, err := book.CommandReceipt(ctx, key+"/reply")
	if err != nil {
		return err
	}
	if replied {
		return book.AcknowledgeCommand(ctx, key, gatewayInputKind, actor, key+"/reply", "gateway-input-reply")
	}
	return book.AcknowledgeCommand(ctx, key, gatewayInputKind, actor, key+"/suppressed", "gateway-input-suppressed")
}

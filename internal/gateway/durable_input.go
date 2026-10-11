package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/work"
)

func (g *Gateway) acceptInput(ctx context.Context, key string, input gatewayInput) error {
	if inputChannel(input) != g.name {
		return errors.New("gateway input belongs to another channel")
	}
	if key == "" || input.Message.MessageID == "" || conversationID(input.Message) == "" {
		return errors.New("gateway durable input requires its original message and conversation")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return g.recoveryLedger.Update(ctx, func(tx *ledger.Tx) error {
		if err := conversation.BindRouteTx(tx, conversationID(input.Message), g.name); err != nil {
			return err
		}
		if err := conversation.BindMessageTx(tx, g.name, input.Message.MessageID, conversationID(input.Message)); err != nil {
			return err
		}
		if input.Message.ChatType == protocol.ChatP2P && input.Message.Origin == "" && g.authority.IsOwnerTx(tx, identity.Account{Channel: g.name, ID: input.Message.ActorID}) {
			if err := tx.PutBinding(nativeAnchorKind, durable.Key(g.name, conversationID(input.Message)), nativeWorkAnchor{Message: input.Message}); err != nil {
				return err
			}
		}
		if err := tx.RecordCommand(key, gatewayInputKind, input.Message.ActorID, raw); err != nil {
			return err
		}
		_, _, err := ingress.AcceptTx(tx, ingress.Input{Kind: "message", Source: g.name, Key: key, Actor: input.Message.ActorID, Conversation: conversationID(input.Message), Payload: raw}, key, time.Now().UTC())
		return err
	})
}

func (g *Gateway) consumeInput(ctx context.Context, book *ledger.Ledger, key string, input gatewayInput, driver RecoveryDriver, claim *inputClaim) error {
	actor := input.Message.ActorID
	ack := func() error { return acknowledgeInput(ctx, book, key, actor) }
	if err := ack(); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("delivery acknowledgement: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	// Without a channel the input can be neither seeded nor answered, so it
	// advances no further and records no new step. Observing an earlier
	// dispatch and recording a silent-listen outcome need no channel, but they
	// wait too: the input stays pending at this one point for a gateway that
	// has a channel, as recoverInput leaves recovery input.
	ch := g.channel()
	if ch == nil {
		return errors.New("gateway reply channel is not available")
	}
	msg := input.Message
	// Remote context reads are retryable, but never an input/dispatch proof.
	// Complete or unknown dispatches are observed without fetching it again.
	_, dispatched, err := book.CommandReceipt(ctx, key+"/dispatch")
	if err != nil {
		return err
	}
	if !dispatched {
		msg = ch.EnrichInput(ctx, msg)
	}
	input.Message = msg
	msg, err = g.prepareWork(ctx, ch, book, key, input)
	if err != nil {
		return err
	}
	input.Message = msg
	guard := g.topicGuard(msg)
	if guard == "" {
		var err error
		msg, err = g.prepareTopic(ctx, ch, book, key, input)
		if err != nil {
			return err
		}
		if err := claim.move(ctx, conversationID(msg)); err != nil {
			return err
		}
	}
	output, ui, err := g.dispatchInput(ctx, ch, book, key, input, msg, guard, driver)
	if ui != nil {
		defer ui.closeProgress()
	}
	if err != nil {
		return err
	}
	if err := g.deliverInput(ctx, ch, book, key, msg, output, ui); err != nil {
		g.retainWorkDeliveryAttention(ctx, book, key, msg, "reply")
		return err
	}
	return ack()
}

func (g *Gateway) dispatchInput(ctx context.Context, ch Channel, book *ledger.Ledger, key string, input gatewayInput, msg channel.Input, guard string, driver RecoveryDriver) (recoveredOutput, *turnUI, error) {
	actor := input.Message.ActorID
	request := g.taskRequest(msg, input.ExpectedTask, nil)
	request.Admission.ExpectedProject = input.ExpectedProject
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
			g.gate.Anchor(conversationID(msg), channel.Address{Channel: g.name, Conversation: conversationID(msg), Message: msg.MessageID})
		}
		if input.Action != nil && input.Action.Action == "turn_retry" {
			recall(ch, input.Action.MessageID)
		}
		ui = g.newTurnUI(ch, msg, silentListen(msg))
		request = g.taskRequest(msg, input.ExpectedTask, ui)
		request.Admission.ExpectedProject = input.ExpectedProject
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
		result, runErr := g.coordinator.Handle(ctx, request)
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
	if err == nil && !output.Recover {
		err = book.Update(context.WithoutCancel(ctx), func(tx *ledger.Tx) error {
			if err := work.CompleteInputTx(tx, request.Address(), output.Result, output.runError()); err != nil {
				return err
			}
			return g.retainWorkOutcomeTx(tx, key, msg, output)
		})
	}
	return output, ui, err
}

func (g *Gateway) deliverInput(ctx context.Context, ch Channel, book *ledger.Ledger, key string, msg channel.Input, output recoveredOutput, ui *turnUI) error {
	// Listening policy can intentionally produce no external message. Record
	// that local disposition separately; it is never a fabricated /reply or
	// evidence that Feishu received a final message.
	if silentListen(msg) && (output.Error != "" || output.Result.Text == "") {
		raw, err := json.Marshal(ledger.CommandProof{CommandID: key, Receipt: "unmentioned-empty-output"})
		if err != nil {
			return err
		}
		return book.RecordCommand(ctx, key+"/suppressed", "gateway-input-suppressed", msg.ActorID, raw)
	}
	runErr := output.runError()
	_, _, err := book.Command(ctx, key+"/reply", "gateway-input-reply", msg.ActorID, func(sendCtx context.Context) (json.RawMessage, error) {
		if err := book.Read(sendCtx, func(tx *ledger.ReadTx) error {
			return work.CheckOutputAuthorityTx(tx, channel.Address{Channel: g.name, Conversation: conversationID(msg), Message: msg.MessageID})
		}); err != nil {
			return nil, err
		}
		if ui == nil {
			ui = g.newResultUI(ch, msg)
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

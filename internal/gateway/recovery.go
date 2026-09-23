package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

const recoveryInputKind = "gateway-recovery-input"

type recoveryInput struct {
	Revival
	Attempt   string               `json:"attempt,omitempty"`
	Prompt    string               `json:"prompt"`
	Notice    string               `json:"notice"`
	Admission task.ResumeAdmission `json:"admission,omitzero"`
}

type RecoveryDriver interface {
	ResumeRetainedChat(context.Context, string, turn.Request) (turn.Result, error)
}

type recoveredOutput struct {
	Result    turn.Result `json:"result"`
	Error     string      `json:"error,omitempty"`
	UserError string      `json:"user_error,omitempty"`
	Canceled  bool        `json:"canceled,omitempty"`
	Recover   bool        `json:"recover,omitempty"`
}

func recoveredResult(result turn.Result, err error) recoveredOutput {
	output := recoveredOutput{Result: result}
	if err != nil {
		output.Error = err.Error()
		output.Canceled = errors.Is(err, context.Canceled)
		var userErr turn.UserError
		if errors.As(err, &userErr) {
			output.UserError = userErr.Text
		}
	}
	return output
}

func (output recoveredOutput) runError() error {
	if output.Canceled {
		return context.Canceled
	}
	if output.UserError != "" {
		return turn.UserError{Text: output.UserError}
	}
	if output.Error != "" {
		return errors.New(output.Error)
	}
	return nil
}

// QueueRecovery accepts either a continuation or delivery of an already
// completed attempt. Acceptance has no external effects and no in-flight
// reservation; the existing command receipt is the sole durable input.
func (g *Gateway) QueueRecovery(ctx context.Context, book *ledger.Ledger, key string, r Revival, completedAttempt string) error {
	if key == "" || r.TaskID == "" || r.ConversationID == "" || r.MessageID == "" || r.Member == "" || r.Requester == "" {
		return errors.New("gateway recovery requires the original task and reply address")
	}
	if completedAttempt != "" {
		owned, err := g.OwnsAttempt(ctx, book, completedAttempt, r.TaskID, r.ConversationID, r.MessageID, r.Requester)
		if err != nil {
			return err
		}
		if owned {
			return nil
		}
	}
	input := recoveryInput{Revival: r, Attempt: completedAttempt,
		Prompt: "@" + r.Member + " " + g.text.T(i18n.ResumePrompt, r.Goal),
		Notice: g.text.T(i18n.ResumeNotice, r.TaskID)}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return book.RecordCommand(ctx, key, recoveryInputKind, r.Requester, raw)
}

// RecoverQueued waits for a bounded recovery pass. Native input is protected
// by Command; unknown dispatch can only recover the matching persisted attempt.
// The running reconciler schedules the same work without waiting below.
func (g *Gateway) RecoverQueued(ctx context.Context, book *ledger.Ledger, driver RecoveryDriver, revive func(string, string) error) error {
	inputs, err := pendingGatewayInputs(ctx, book)
	if err != nil {
		return err
	}
	var result error
	batches := recoveryBatches(inputs)
	var mu sync.Mutex
	var workers sync.WaitGroup
	next := 0
	for range min(cap(g.slots), len(batches)) {
		workers.Go(func() {
			for {
				mu.Lock()
				if next == len(batches) {
					mu.Unlock()
					return
				}
				batch := batches[next]
				next++
				mu.Unlock()
				for _, receipt := range batch {
					if err := g.recoverQueuedInput(ctx, book, receipt, driver, revive); err != nil {
						mu.Lock()
						result = errors.Join(result, fmt.Errorf("gateway recovery %s: %w", receipt.ID, err))
						mu.Unlock()
					}
				}
			}
		})
	}
	workers.Wait()
	return result
}

func recoveryBatches(inputs []ledger.CommandRecord) [][]ledger.CommandRecord {
	var batches [][]ledger.CommandRecord
	conversations := map[string]int{}
	for _, receipt := range inputs {
		var input recoveryInput
		if receipt.Kind == gatewayInputKind {
			ordinary, _ := decodeGatewayInput(receipt)
			input.ConversationID = conversationID(ordinary.Message)
		} else {
			_ = json.Unmarshal(receipt.Result, &input)
		}
		if input.ConversationID == "" {
			batches = append(batches, []ledger.CommandRecord{receipt})
			continue
		}
		index, found := conversations[input.ConversationID]
		if !found {
			index = len(batches)
			conversations[input.ConversationID] = index
			batches = append(batches, nil)
		}
		batches[index] = append(batches[index], receipt)
	}
	return batches
}

// RecoveryWorkers is the application's existing lifetime owner. Go rejects
// new work after shutdown closes admission, before joining admitted workers.
type RecoveryWorkers interface {
	Go(func()) bool
}

// ReconcileQueued schedules only inputs with available capacity and returns
// without waiting for native turns or retained observers. Execution and error
// semantics are shared with the synchronous RecoverQueued/DispatchResume path.
func (g *Gateway) ReconcileQueued(ctx context.Context, book *ledger.Ledger, driver RecoveryDriver, revive func(string, string) error, workers RecoveryWorkers) error {
	inputs, err := pendingGatewayInputs(ctx, book)
	if err != nil {
		return err
	}
	g.forgetSettledReasons(inputs)
	// A failed/unknown head receipt remains pending. Rotate opportunities so
	// it cannot monopolize every available slot on every runtime pass.
	g.mu.Lock()
	after := g.recoveryAfter
	g.mu.Unlock()
	start := 0
	for i, receipt := range inputs {
		if receipt.ID == after {
			start = i + 1
			break
		}
	}
	scheduled := 0
	for i := range inputs {
		receipt := inputs[(start+i)%len(inputs)]
		run, release, err := g.claimQueued(ctx, book, receipt, driver, revive, false)
		if errors.Is(err, channel.ErrDeliveryQueued) {
			continue
		}
		if err != nil {
			return err
		}
		if !workers.Go(func() {
			defer release()
			if err := run(); ctx.Err() == nil {
				g.reportPending("gateway recovery remains pending", receipt.ID, err)
			}
		}) {
			release()
			return errors.New("gateway recovery worker admission is closed")
		}
		g.mu.Lock()
		g.recoveryAfter = receipt.ID
		g.mu.Unlock()
		scheduled++
		if scheduled == cap(g.slots) {
			break
		}
	}
	return nil
}

// reportPending logs why an accepted input is still pending when the reason
// changes, and forgets it once the input goes through.
func (g *Gateway) reportPending(message, id string, err error) {
	g.mu.Lock()
	if err == nil {
		delete(g.pendingReasons, id)
		g.mu.Unlock()
		return
	}
	reason := err.Error()
	repeated := g.pendingReasons[id] == reason
	if g.pendingReasons == nil {
		g.pendingReasons = map[string]string{}
	}
	g.pendingReasons[id] = reason
	g.mu.Unlock()
	if !repeated {
		slog.Error(message, "input", id, "error", err)
	}
}

// forgetSettledReasons drops reasons remembered for inputs that are no
// longer pending, whichever path settled them.
func (g *Gateway) forgetSettledReasons(pending []ledger.CommandRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pendingReasons) == 0 {
		return
	}
	live := make(map[string]bool, len(pending))
	for _, receipt := range pending {
		live[receipt.ID] = true
	}
	for id := range g.pendingReasons {
		if !live[id] {
			delete(g.pendingReasons, id)
		}
	}
}

func (g *Gateway) recoverQueuedInput(ctx context.Context, book *ledger.Ledger, receipt ledger.CommandRecord, driver RecoveryDriver, revive func(string, string) error) error {
	run, release, err := g.claimQueued(ctx, book, receipt, driver, revive, true)
	if err != nil {
		return err
	}
	defer release()
	return run()
}

func (g *Gateway) recoverAcceptedInput(ctx context.Context, book *ledger.Ledger, receipt ledger.CommandRecord, driver RecoveryDriver, revive func(string, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var input recoveryInput
	if receipt.Kind != recoveryInputKind || receipt.FinishedAt == nil || receipt.Error != "" ||
		json.Unmarshal(receipt.Result, &input) != nil || input.Requester != receipt.Actor {
		return errors.New("gateway recovery has invalid acceptance")
	}
	// A reply may already be durably confirmed while projection
	// acknowledgement failed. Retry only the acknowledgement then.
	err := book.AcknowledgeCommand(ctx, receipt.ID, recoveryInputKind, input.Requester, receipt.ID+"/reply", "gateway-recovery-reply")
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("acknowledgement: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	return g.recoverInput(ctx, book, receipt.ID, input, driver, revive)
}

func (g *Gateway) recoverInput(ctx context.Context, book *ledger.Ledger, key string, input recoveryInput, driver RecoveryDriver, revive func(string, string) error) error {
	sender, ok := g.ch.(textReplier)
	if !ok {
		return errors.New("gateway recovery reply channel is not available")
	}
	anchor := input.MessageID
	request := turn.Request{Channel: "feishu", ConversationID: input.ConversationID, ExpectedTask: input.TaskID,
		MessageID: anchor, ChatID: input.ChatID, SenderOpenID: input.Requester, ChatType: protocol.ParseChatType(input.ChatType), Mentioned: true, Input: input.Prompt, ResumeAdmission: input.Admission}
	var output recoveredOutput
	if input.Attempt != "" {
		if driver == nil {
			return errors.New("gateway retained result recovery is unavailable")
		}
		result, err := driver.ResumeRetainedChat(ctx, input.Attempt, request)
		if err != nil && result.Attempt == "" {
			return err
		}
		if result.Attempt != input.Attempt {
			return fmt.Errorf("%w: retained result belongs to another attempt", channel.ErrOutcomeUnknown)
		}
		output = recoveredResult(result, err)
	} else {
		receipt, dispatched, err := book.CommandReceipt(ctx, key+"/dispatch")
		if err != nil {
			return err
		}
		if dispatched && (receipt.Kind != "gateway-recovery-dispatch" || receipt.Actor != input.Requester) {
			return fmt.Errorf("%w: dispatch command has another identity", ledger.ErrConflict)
		}
		if !dispatched && input.Admission != (task.ResumeAdmission{}) {
			if err := book.Read(ctx, func(tx *ledger.ReadTx) error { return task.CheckResumeAdmissionTx(tx, input.Admission) }); err != nil {
				return err
			}
		}
		if err := revive(input.ConversationID, input.Member); err != nil {
			return err
		}
		anchor, err = g.recoveryNotice(ctx, book, key, input, sender)
		if err != nil {
			return err
		}
		request.MessageID = anchor
		raw, replayed, err := book.Command(ctx, key+"/dispatch", "gateway-recovery-dispatch", input.Requester, func(ctx context.Context) (json.RawMessage, error) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var acceptanceErr error
			var admitted string
			request.OnTurnReady = func(taskID, attemptID string) {
				admitted = attemptID
				acceptanceErr = rememberRecoveryAttempt(ctx, book, key, input.Requester, request, taskID, attemptID)
				if acceptanceErr != nil {
					cancel()
				}
			}
			result, runErr := g.processor.Handle(ctx, request)
			runErr = errors.Join(runErr, acceptanceErr)
			output := recoveredResult(result, runErr)
			// An execution layer block arrives here only through turn's
			// planRecoveryError, stripped of its attempt and marked
			// ErrStopUnconfirmed (see agentexec.RecoveryBlocked).
			var blocked *agentexec.RecoveryBlocked
			output.Recover = errors.As(runErr, &blocked) || errors.Is(runErr, harness.ErrStopUnconfirmed) ||
				admitted != "" && result.Attempt != admitted
			// A completed observation can carry an execution error. Persist
			// that output as a processing receipt, not a failed command that
			// would needlessly re-observe an already completed execution.
			return json.Marshal(output)
		})
		if err != nil && !replayed {
			return err
		}
		if err != nil || json.Unmarshal(raw, &output) != nil || output.Recover {
			output, err = recoverDispatchedAttempt(ctx, book, key, input.Requester, request, driver)
			if err != nil {
				return err
			}
		}
	}
	// Save only the delivery outcome here. A failure or crash during the
	// external reply remains unknown; neither input nor reply is replayed.
	_, _, err := book.Command(ctx, key+"/reply", "gateway-recovery-reply", input.Requester, func(ctx context.Context) (json.RawMessage, error) {
		text := output.Result.Text
		if text == "" {
			text = output.Error
		}
		id, err := sender.ReplyText(ctx, anchor, g.truncateRunes(text, maxReplyRunes))
		if err != nil {
			return nil, noticeError(err)
		}
		if id == "" {
			return nil, channel.ErrOutcomeUnknown
		}
		return json.Marshal(ledger.CommandProof{CommandID: key, Receipt: id})
	})
	if err != nil {
		return fmt.Errorf("result delivery requires reconciliation: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	return book.AcknowledgeCommand(ctx, key, recoveryInputKind, input.Requester, key+"/reply", "gateway-recovery-reply")
}

func (g *Gateway) recoveryNotice(ctx context.Context, book *ledger.Ledger, key string, input recoveryInput, sender textReplier) (string, error) {
	raw, _, err := book.Command(ctx, key+"/notice", "gateway-recovery-notice", input.Requester, func(ctx context.Context) (json.RawMessage, error) {
		if !input.Manual {
			if recall, ok := g.ch.(recaller); ok {
				for _, id := range append([]string{input.OpenCard}, input.Interim...) {
					if id != "" {
						if err := recall.DeleteMessage(ctx, id); err != nil {
							return nil, noticeError(err)
						}
					}
				}
			}
		}
		id, err := sender.ReplyText(ctx, input.MessageID, input.Notice)
		if err != nil {
			return nil, noticeError(err)
		}
		if id == "" {
			return nil, channel.ErrOutcomeUnknown
		}
		return json.Marshal(id)
	})
	if err != nil {
		return "", fmt.Errorf("notice receipt requires reconciliation: %w", errors.Join(channel.ErrOutcomeUnknown, err))
	}
	var anchor string
	if json.Unmarshal(raw, &anchor) != nil || anchor == "" {
		return "", errors.New("gateway recovery notice has no durable message identity")
	}
	return anchor, nil
}

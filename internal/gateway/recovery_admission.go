package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// SetRecoveryLedger wires the durable input owner before serving messages.
func (g *Gateway) SetRecoveryLedger(book *ledger.Ledger) { g.recoveryLedger = book }

// QueueTaskResume accepts dormant input. Only the task owner's exact grant can
// later authorize it; acceptance and a wake-up are not execution permissions.
func (g *Gateway) QueueTaskResume(ctx context.Context, book *ledger.Ledger, key string, r Revival, admission task.ResumeAdmission) error {
	if book == nil || key == "" || !admission.Valid() || admission.TaskID != r.TaskID ||
		r.ConversationID == "" || r.MessageID == "" || r.Member == "" || r.Requester == "" {
		return errors.New("gateway resume requires a stable input, task admission and reply address")
	}
	if g.ch == nil {
		return errors.New("gateway resume channel cannot post notices")
	}
	input := recoveryInput{Revival: r, Admission: admission,
		Prompt: "@" + r.Member + " " + g.text.T(i18n.TaskResumeManual, r.Goal),
		Notice: g.text.T(i18n.TaskResumeNotice, r.TaskID)}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return book.RecordCommand(ctx, key, recoveryInputKind, r.Requester, raw)
}

// DispatchResume wakes exactly one granted manual input. Startup inputs remain
// behind the application's complete accounting pass, even when another task's
// resume is accepted concurrently.
func (g *Gateway) DispatchResume(ctx context.Context, book *ledger.Ledger, admission task.ResumeAdmission, driver RecoveryDriver, revive func(string, string) error) error {
	if !admission.Valid() {
		return task.ErrExecutionStopped
	}
	receipt, exists, err := book.CommandReceipt(ctx, admission.ID)
	if err != nil {
		return err
	}
	var input recoveryInput
	if !exists || json.Unmarshal(receipt.Result, &input) != nil || input.Admission != admission || input.TaskID != admission.TaskID {
		return fmt.Errorf("%w: gateway resume input does not match its admission", task.ErrExecutionStopped)
	}
	// Wakes are best-effort and never wait for capacity.
	// The durable accepted input remains pending for the runtime scheduler.
	release, err := g.claimRecovery(ctx, receipt)
	if err != nil {
		return err
	}
	defer release()
	return g.recoverAcceptedInput(ctx, book, receipt, driver, revive)
}

type recoveryAttempt struct {
	InputID      string `json:"input_id"`
	TaskID       string `json:"task_id"`
	AttemptID    string `json:"attempt_id"`
	Conversation string `json:"conversation"`
	MessageID    string `json:"message_id"`
}

// rememberRecoveryAttempt records the dispatch's admitted identity before
// native input. It is a processing receipt, never another executable intent.
func rememberRecoveryAttempt(ctx context.Context, book *ledger.Ledger, key, actor string, req turn.Request, taskID, attemptID string) error {
	if taskID == "" || attemptID == "" || req.ExpectedTask != "" && req.ExpectedTask != taskID {
		return errors.New("gateway dispatch returned another task or an empty attempt")
	}
	raw, err := json.Marshal(recoveryAttempt{InputID: key, TaskID: taskID, AttemptID: attemptID, Conversation: req.ConversationID, MessageID: req.MessageID})
	if err != nil {
		return err
	}
	return book.RecordCommand(ctx, key+"/attempt", "gateway-input-attempt", actor, raw)
}

func recoverDispatchedAttempt(ctx context.Context, book *ledger.Ledger, key, actor string, req turn.Request, driver RecoveryDriver) (recoveredOutput, error) {
	if driver == nil {
		return recoveredOutput{}, errors.New("gateway retained result recovery is unavailable")
	}
	receipt, found, err := book.CommandReceipt(ctx, key+"/attempt")
	if err != nil {
		return recoveredOutput{}, err
	}
	var admitted recoveryAttempt
	if !found || receipt.FinishedAt == nil || receipt.Error != "" || receipt.Kind != "gateway-input-attempt" || receipt.Actor != actor ||
		json.Unmarshal(receipt.Result, &admitted) != nil || admitted.InputID != key || admitted.TaskID == "" || admitted.AttemptID == "" ||
		admitted.Conversation != req.ConversationID || admitted.MessageID != req.MessageID || req.ExpectedTask != "" && admitted.TaskID != req.ExpectedTask {
		return recoveredOutput{}, fmt.Errorf("%w: dispatch lacks its original admitted identity", channel.ErrOutcomeUnknown)
	}
	req.ExpectedTask = admitted.TaskID
	result, runErr := driver.ResumeRetainedChat(ctx, admitted.AttemptID, req)
	if runErr != nil && result.Attempt == "" {
		return recoveredOutput{}, runErr
	}
	if result.Attempt != admitted.AttemptID {
		return recoveredOutput{}, fmt.Errorf("%w: dispatch recovery returned another attempt", channel.ErrOutcomeUnknown)
	}
	return recoveredResult(result, runErr), nil
}

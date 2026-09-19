package gateway

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const invalidAttemptReceipt = "!invalid"
const gatewayInputKind = "gateway-input"

type gatewayInput struct {
	Message      feishu.InboundMessage `json:"message"`
	ExpectedTask string                `json:"expected_task,omitempty"`
}

const attemptInputQuery = `SELECT id FROM commands
	WHERE kind='gateway-input-attempt' AND steve_gateway_attempt_v1(result) IN (?, '!invalid')`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_gateway_attempt_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		raw, ok := args[0].(string)
		var proof recoveryAttempt
		if !ok || json.Unmarshal([]byte(raw), &proof) != nil || proof.AttemptID == "" || proof.InputID == "" || proof.TaskID == "" || proof.MessageID == "" || proof.Conversation == "" {
			return invalidAttemptReceipt, nil
		}
		return proof.AttemptID, nil
	})
	ledger.MustRegisterReadIndex("commands_gateway_attempt", `CREATE INDEX IF NOT EXISTS commands_gateway_attempt ON commands(steve_gateway_attempt_v1(result)) WHERE kind='gateway-input-attempt'`)
}

// OwnsAttempt identifies the single accepted input responsible for this result.
// It is a derived lookup over the original admission receipt, not a new intent.
// Malformed owner receipts remain candidates so absence cannot be guessed.
func (g *Gateway) OwnsAttempt(ctx context.Context, book *ledger.Ledger, attemptID, taskID, conversation, messageID, requester string) (bool, error) {
	if book == nil || attemptID == "" || taskID == "" || conversation == "" || messageID == "" || requester == "" {
		return false, errors.New("gateway result ownership requires the original attempt and address")
	}
	rows, err := book.DB().QueryContext(ctx, attemptInputQuery, attemptID)
	if err != nil {
		return false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	found := false
	for _, id := range ids {
		receipt, exists, err := book.CommandReceipt(ctx, id)
		if err != nil {
			return false, err
		}
		var proof recoveryAttempt
		if !exists || receipt.Kind != "gateway-input-attempt" || receipt.Actor != requester || receipt.FinishedAt == nil || receipt.Error != "" || json.Unmarshal(receipt.Result, &proof) != nil ||
			proof.AttemptID != attemptID || proof.TaskID != taskID || proof.Conversation != conversation || proof.MessageID != messageID ||
			id != proof.InputID+"/attempt" {
			return false, fmt.Errorf("gateway attempt receipt %s has inconsistent identity", id)
		}
		input, exists, err := book.CommandReceipt(ctx, proof.InputID)
		if err != nil {
			return false, err
		}
		if !exists || input.FinishedAt == nil || input.Error != "" || input.Actor != receipt.Actor ||
			input.Kind != gatewayInputKind && input.Kind != recoveryInputKind {
			return false, errors.New("gateway result has no successful original acceptance")
		}
		if err := verifyAttemptInput(ctx, book, input, proof); err != nil {
			return false, err
		}
		if found {
			return false, errors.New("multiple gateway inputs claim the same execution")
		}
		found = true
	}
	return found, nil
}

func verifyAttemptInput(ctx context.Context, book *ledger.Ledger, receipt ledger.CommandRecord, proof recoveryAttempt) error {
	if receipt.Kind == gatewayInputKind {
		var input gatewayInput
		if json.Unmarshal(receipt.Result, &input) != nil || input.Message.MessageID != proof.MessageID ||
			conversationID(input.Message) != proof.Conversation || input.Message.SenderOpenID != receipt.Actor ||
			input.ExpectedTask != "" && input.ExpectedTask != proof.TaskID {
			return errors.New("gateway attempt is not linked to its accepted message")
		}
		return nil
	}
	var input recoveryInput
	if json.Unmarshal(receipt.Result, &input) != nil || input.Attempt != "" || input.TaskID != proof.TaskID ||
		input.ConversationID != proof.Conversation || input.Requester != receipt.Actor || input.MessageID == "" {
		return errors.New("gateway attempt is not linked to its accepted recovery input")
	}
	// Recovery changes the reply anchor through its successful notice. The
	// original input anchor alone cannot prove which attempt it dispatched.
	notice, exists, err := book.CommandReceipt(ctx, receipt.ID+"/notice")
	if err != nil {
		return err
	}
	var anchor string
	if !exists || notice.Kind != "gateway-recovery-notice" || notice.Actor != receipt.Actor ||
		notice.FinishedAt == nil || notice.Error != "" || json.Unmarshal(notice.Result, &anchor) != nil || anchor != proof.MessageID {
		return errors.New("gateway attempt has no matching successful notice anchor")
	}
	return nil
}

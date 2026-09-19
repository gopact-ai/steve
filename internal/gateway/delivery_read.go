package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

const deliveryAttemptSQL = `SELECT count(*),min(id) FROM
	(SELECT id FROM commands INDEXED BY commands_gateway_attempt
	WHERE kind='gateway-input-attempt' AND steve_gateway_attempt_v1(result) IN (?, '!invalid') LIMIT 2)`

// ConfirmedAttemptDeliveryTx verifies the owner's exact accepted-input,
// original-attempt and externally confirmed reply chain in one snapshot.
// acknowledged_by is only a queue projection, never delivery authority.
func ConfirmedAttemptDeliveryTx(tx ledger.Reader, attemptID, taskID, conversation, turnID, requester string) (string, bool, error) {
	if attemptID == "" || taskID == "" || conversation == "" || turnID == "" || requester == "" {
		return "", false, errors.New("gateway delivery requires its original execution and address")
	}
	var count int
	var id sql.NullString
	if err := tx.QueryRow(deliveryAttemptSQL, attemptID).Scan(&count, &id); err != nil {
		return "", false, err
	}
	if count == 0 {
		return "", false, nil
	}
	if count != 1 || !id.Valid {
		return "", false, errors.New("gateway delivery has ambiguous attempt ownership")
	}
	owner, found, err := ledger.CommandReceiptTx(tx, id.String)
	if err != nil {
		return "", false, err
	}
	var proof recoveryAttempt
	if !found || owner.Kind != "gateway-input-attempt" || owner.Actor != requester || owner.FinishedAt == nil ||
		owner.Error != "" || json.Unmarshal(owner.Result, &proof) != nil || proof.InputID == "" ||
		proof.AttemptID != attemptID || proof.TaskID != taskID || proof.Conversation != conversation ||
		proof.MessageID != turnID || owner.ID != proof.InputID+"/attempt" {
		return "", false, errors.New("gateway delivery has inconsistent original attempt evidence")
	}
	input, found, err := ledger.CommandReceiptTx(tx, proof.InputID)
	if err != nil {
		return "", false, err
	}
	if !found || input.FinishedAt == nil || input.Error != "" || input.Actor != requester {
		return "", false, errors.New("gateway delivery lacks successful input acceptance")
	}
	if err := verifyAttemptInputTx(tx, input, proof); err != nil {
		return "", false, err
	}
	reply, found, err := ledger.CommandReceiptTx(tx, proof.InputID+"/reply")
	if err != nil || !found {
		return "", false, err
	}
	kind := "gateway-input-reply"
	if input.Kind == recoveryInputKind {
		kind = "gateway-recovery-reply"
	}
	var delivered ledger.CommandProof
	if reply.Kind != kind || reply.Actor != requester || reply.FinishedAt == nil || reply.Error != "" ||
		json.Unmarshal(reply.Result, &delivered) != nil || delivered.CommandID != proof.InputID || delivered.Receipt == "" {
		return "", false, errors.New("gateway delivery lacks exact confirmed reply proof")
	}
	return delivered.Receipt, true, nil
}

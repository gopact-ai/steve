package console

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

// ConfirmedAttemptDeliveryTx reads the original exchange's server-generated
// completion in the caller's committed snapshot. UI transcript rows, client
// command IDs, progress and optional Changes are not delivery evidence.
func ConfirmedAttemptDeliveryTx(tx ledger.Reader, attemptID, conversation, turnID string) (string, bool, error) {
	id, ok := strings.CutPrefix(turnID, AnchorMark)
	if !ok || id == "" || attemptID == "" || conversation == "" {
		return "", false, errors.New("console delivery requires its original attempt and anchor")
	}
	var raw []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, consoleExchangeKind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value, err := decodeConsoleRecord(consoleExchangeKind, raw)
	if err != nil {
		return "", false, err
	}
	row := value.(consoleExchangeRecord)
	if row.ID != id || row.Conversation != conversation {
		return "", false, errors.New("console delivery exchange identity differs")
	}
	if row.State != consoleapi.ExchangeDone && row.State != consoleapi.ExchangeFailed || row.Receipt == nil {
		return "", false, nil
	}
	reply := row.Receipt
	if reply.ID == "" || reply.ID != row.ReplyID || reply.ExchangeID != id || reply.Conversation != conversation ||
		reply.AttemptID != attemptID || reply.Kind != "reply" || reply.Silent ||
		(row.State == consoleapi.ExchangeFailed) != (reply.Error != "") {
		return "", false, errors.New("console delivery differs from its original native completion")
	}
	return reply.ID, true, nil
}

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

// The receipt owner proves delivery from committed records in the same snapshot
// as the attempt's acknowledgement. In-memory terminal state is not evidence.
func abandonDeliveryEvidenceTx(tx ledger.Reader, r attempt.Record) (attempt.AbandonDelivery, error) {
	if r.Abandoned == nil || r.Abandoned.ProjectedAt.IsZero() {
		return "", errors.New("abandonment retirement has not committed")
	}
	if r.Kind != attempt.KindChat && r.Kind != attempt.KindPlan || !IsConsole(r.Abandoned.Conversation) || !strings.HasPrefix(r.Abandoned.MessageID, AnchorMark) {
		return attempt.AbandonDeliveryNotRequired, nil
	}
	var raw []byte
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id='state'`, consoleStoreKind).Scan(&raw); err != nil {
		return "", err
	}
	var control consoleControl
	if err := json.Unmarshal(raw, &control); err != nil || control.Revision == 0 {
		return "", errors.New("console delivery receiver has no valid committed control")
	}
	id := strings.TrimPrefix(r.Abandoned.MessageID, AnchorMark)
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, consoleExchangeKind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return missingAbandonExchangeTx(tx, r.Abandoned.Conversation, id)
	}
	if err != nil {
		return "", err
	}
	value, err := decodeConsoleRecord(consoleExchangeKind, raw)
	if err != nil {
		return "", err
	}
	e := value.(consoleExchangeRecord)
	if e.ID != id || e.Conversation != r.Abandoned.Conversation {
		return "", attempt.ErrAbandonInput
	}
	original, err := attempt.AbandonInputTx(tx, r)
	if err != nil || original != r.Abandoned.MessageID {
		return "", errors.Join(attempt.ErrAbandonInput, err)
	}
	if !e.State.Terminal() {
		return "", errAbandonDeliveryPending
	}
	if e.RecoveryAbandon == nil || e.RecoveryAbandon.AttemptID != r.ID {
		return attempt.AbandonDeliveryTerminal, nil
	}
	if e.State != consoleapi.ExchangeCancelled || e.Receipt == nil || e.Receipt.ID == "" || e.ReplyID != e.Receipt.ID || e.Receipt.AttemptID != r.ID || e.Receipt.Conversation != e.Conversation || e.Receipt.ExchangeID != e.ID || e.Receipt.Kind != "reply" || e.Receipt.Text != e.RecoveryAbandon.Text {
		return "", errors.New("abandonment reply differs from its terminal receipt")
	}
	// Exact keyed receipts outlive bounded transcript pruning. If its reply is
	// still present, it must be the same persisted value, not another response.
	err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, consoleReplyKind, e.Receipt.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.AbandonDelivered, nil
	}
	if err != nil {
		return "", err
	}
	value, err = decodeConsoleRecord(consoleReplyKind, raw)
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(value.(consoleReplyRecord).Reply, *e.Receipt) {
		return "", errors.New("abandonment transcript reply differs from its receipt")
	}
	return attempt.AbandonDelivered, nil
}

func missingAbandonExchangeTx(tx ledger.Reader, conversation, id string) (attempt.AbandonDelivery, error) {
	var raw []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, consoleHeadKind, conversation).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.AbandonDeliveryGone, nil
	}
	if err != nil {
		return "", err
	}
	value, err := decodeConsoleRecord(consoleHeadKind, raw)
	if err != nil {
		return "", err
	}
	if value.(consoleHead).FirstExchange == id {
		return "", errors.New("console delivery head refers to a missing input")
	}
	return attempt.AbandonDeliveryGone, nil
}

var errAbandonDeliveryPending = errors.New("original abandonment input is not durably finished")

func (s *Service) abandonDeliveryStatus(ctx context.Context, r attempt.Record) error {
	if r.Kind != attempt.KindChat && r.Kind != attempt.KindPlan || !IsConsole(r.Abandoned.Conversation) || !strings.HasPrefix(r.Abandoned.MessageID, AnchorMark) {
		return nil
	}
	s.mu.Lock()
	book := s.book
	s.mu.Unlock()
	if book == nil {
		return errors.New("abandonment delivery requires durable console records")
	}
	return book.Read(ctx, func(tx *ledger.ReadTx) error {
		_, err := abandonDeliveryEvidenceTx(tx, r)
		return err
	})
}

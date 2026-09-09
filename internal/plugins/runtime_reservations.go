package plugins

import (
	"context"
	"github.com/gopact-ai/steve/internal/ledger"
)

const runtimeReservationKind = "plugin-runtime-reservation"

type RuntimeReservation struct {
	AttemptID string    `json:"attempt_id"`
	Selection Selection `json:"selection"`
	RuntimeID string    `json:"runtime_id,omitempty"`
}

func (l *Library) ReserveRuntime(ctx context.Context, attemptID string, selection Selection) error {
	if attemptID == "" {
		return ErrInvalid
	}
	wanted, err := selection.Hash()
	if err != nil {
		return err
	}
	return l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		records, err := tx.Bindings(runtimeReservationKind)
		if err != nil {
			return err
		}
		if raw, ok := records[attemptID]; ok {
			var existing RuntimeReservation
			if err := decodeStrict(raw, &existing); err != nil {
				return err
			}
			actual, err := existing.Selection.Hash()
			if err != nil || actual != wanted {
				return ErrConflict
			}
			return nil
		}
		return tx.PutBinding(runtimeReservationKind, attemptID, RuntimeReservation{AttemptID: attemptID, Selection: selection})
	})
}
func (l *Library) BindRuntime(ctx context.Context, attemptID string, ref RuntimeRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	wanted, _ := ref.Selection.Hash()
	return l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		records, err := tx.Bindings(runtimeReservationKind)
		if err != nil {
			return err
		}
		raw, ok := records[attemptID]
		if !ok {
			return ErrUnavailable
		}
		var existing RuntimeReservation
		if err := decodeStrict(raw, &existing); err != nil {
			return err
		}
		actual, err := existing.Selection.Hash()
		if err != nil || actual != wanted || (existing.RuntimeID != "" && existing.RuntimeID != ref.ID) {
			return ErrConflict
		}
		existing.RuntimeID = ref.ID
		return tx.PutBinding(runtimeReservationKind, attemptID, existing)
	})
}
func (l *Library) RuntimeReservation(ctx context.Context, attemptID string) (RuntimeReservation, bool, error) {
	var result RuntimeReservation
	found, err := l.Ledger.GetBinding(ctx, runtimeReservationKind, attemptID, &result)
	return result, found, err
}

package plugins

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

const managementOperationKind = "plugin-management"
const OperationPending = "pending"
const OperationSucceeded = "succeeded"

type Operation struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Fingerprint string    `json:"fingerprint"`
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (l *Library) BeginOperation(ctx context.Context, id, kind string, input any) (Operation, error) {
	if !nameShape.MatchString(id) || !nameShape.MatchString(kind) {
		return Operation{}, ErrInvalid
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return Operation{}, err
	}
	fingerprint := contentDigest(raw)
	operation := Operation{ID: id, Kind: kind, Fingerprint: fingerprint, State: OperationPending, UpdatedAt: time.Now().UTC()}
	err = l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings(managementOperationKind)
		if err != nil {
			return err
		}
		if prior, exists := rows[id]; exists {
			var original Operation
			if err := decodeStrict(prior, &original); err != nil {
				return err
			}
			if original.ID != id || original.Kind != kind || original.Fingerprint != fingerprint {
				return ErrConflict
			}
			operation = original
			return nil
		}
		return tx.PutBinding(managementOperationKind, id, operation)
	})
	return operation, err
}

func (l *Library) FinishOperation(ctx context.Context, operation Operation, cause error) error {
	if !nameShape.MatchString(operation.ID) || !digestShape.MatchString(operation.Fingerprint) {
		return ErrInvalid
	}
	return l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings(managementOperationKind)
		if err != nil {
			return err
		}
		var current Operation
		if raw, exists := rows[operation.ID]; !exists {
			return ErrUnavailable
		} else if err := decodeStrict(raw, &current); err != nil {
			return err
		}
		if current.Fingerprint != operation.Fingerprint || current.Kind != operation.Kind {
			return ErrConflict
		}
		if current.State == OperationSucceeded {
			return nil
		}
		current.UpdatedAt = time.Now().UTC()
		if cause != nil {
			current.Error = cause.Error()
		} else {
			current.State = OperationSucceeded
			current.Error = ""
		}
		return tx.PutBinding(managementOperationKind, current.ID, current)
	})
}

func (l *Library) Operations(ctx context.Context) ([]Operation, error) {
	rows, err := l.Ledger.Bindings(ctx, managementOperationKind)
	if err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(rows))
	for _, id := range sortedKeys(rows) {
		var operation Operation
		if err := decodeStrict(rows[id], &operation); err != nil {
			return nil, err
		}
		if operation.ID != id || !digestShape.MatchString(operation.Fingerprint) {
			return nil, ErrIntegrity
		}
		out = append(out, operation)
	}
	return out, nil
}

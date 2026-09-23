package pluginledger

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/plugins"

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
	if !plugins.ValidName(id) || !plugins.ValidName(kind) {
		return Operation{}, plugins.ErrInvalid
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return Operation{}, err
	}
	fingerprint := plugins.ContentDigest(raw)
	operation := Operation{ID: id, Kind: kind, Fingerprint: fingerprint, State: OperationPending, UpdatedAt: time.Now().UTC()}
	err = l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings(managementOperationKind)
		if err != nil {
			return err
		}
		if prior, exists := rows[id]; exists {
			var original Operation
			if err := plugins.DecodeStrict(prior, &original); err != nil {
				return err
			}
			if original.ID != id || original.Kind != kind || original.Fingerprint != fingerprint {
				return plugins.ErrConflict
			}
			operation = original
			return nil
		}
		return tx.PutBinding(managementOperationKind, id, operation)
	})
	return operation, err
}

func (l *Library) FinishOperation(ctx context.Context, operation Operation, cause error) error {
	if !plugins.ValidName(operation.ID) || !plugins.ValidDigest(operation.Fingerprint) {
		return plugins.ErrInvalid
	}
	return l.Ledger.Update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings(managementOperationKind)
		if err != nil {
			return err
		}
		var current Operation
		if raw, exists := rows[operation.ID]; !exists {
			return plugins.ErrUnavailable
		} else if err := plugins.DecodeStrict(raw, &current); err != nil {
			return err
		}
		if current.Fingerprint != operation.Fingerprint || current.Kind != operation.Kind {
			return plugins.ErrConflict
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
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		var operation Operation
		if err := plugins.DecodeStrict(rows[id], &operation); err != nil {
			return nil, err
		}
		if operation.ID != id || !plugins.ValidDigest(operation.Fingerprint) {
			return nil, plugins.ErrIntegrity
		}
		out = append(out, operation)
	}
	return out, nil
}

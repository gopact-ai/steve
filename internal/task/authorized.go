package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// SpawnAuthorized fixes the parent execution at tool admission. Its grant
// guard and parent epoch are rechecked in the transaction that creates the child.
func (s *Store) SpawnAuthorized(ctx context.Context, token ExecutionToken, child Task, guard func(*ledger.Tx) error) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return Task{}, err
	}
	return s.spawnLocked(token.TaskID, child, func(next data) error { return s.replaceAuthorizedLocked(ctx, token, next, guard) })
}

// CheckAuthorized checks a previously admitted execution and consumer grant
// without replacing it with the task's current execution epoch.
func (s *Store) CheckAuthorized(ctx context.Context, token ExecutionToken, guard func(*ledger.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book == nil {
		return errors.New("authorized task operation requires the ledger")
	}
	return s.book.Update(ctx, func(tx *ledger.Tx) error {
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		return CheckExecutionTx(tx, &token)
	})
}

func (s *Store) replaceAuthorizedLocked(ctx context.Context, token ExecutionToken, next data, guard func(*ledger.Tx) error) error {
	if s.book == nil {
		return errors.New("authorized task operation requires the ledger")
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	expected, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	err = s.book.Update(ctx, func(tx *ledger.Tx) error {
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		if err := CheckExecutionTx(tx, &token); err != nil {
			return err
		}
		actual, found, err := tx.LoadDocument("tasks")
		if err != nil {
			return err
		}
		var currentJSON, expectedJSON bytes.Buffer
		if !found || json.Compact(&currentJSON, actual) != nil || json.Compact(&expectedJSON, expected) != nil || !bytes.Equal(currentJSON.Bytes(), expectedJSON.Bytes()) {
			return ledger.ErrConflict
		}
		return tx.StoreDocument("tasks", raw)
	})
	if err != nil {
		return err
	}
	s.installLocked(next)
	return nil
}

// SetDeliveryAuthorized records consumption under the original parent's grant.
func (s *Store) SetDeliveryAuthorized(ctx context.Context, token ExecutionToken, childID, state string, guard func(*ledger.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return err
	}
	_, ok := s.data.Tasks[childID]
	if !ok {
		return errors.New("delegated child is missing")
	}
	lineage, err := taskLineage(s.data.Tasks, childID)
	if err != nil {
		return err
	}
	belongs := false
	for _, entry := range lineage {
		if entry.ID == token.TaskID {
			belongs = true
			break
		}
	}
	if !belongs || childID == token.TaskID {
		return errors.New("child belongs to another parent execution")
	}
	next := s.clone()
	next.Tasks[childID].Delivery = &Delivery{State: state, Key: DeliveryKey(childID), At: s.now()}
	return s.replaceAuthorizedLocked(ctx, token, next, guard)
}

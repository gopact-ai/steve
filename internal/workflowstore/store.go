// Package workflowstore keeps gopact workflow state in the application's
// replicated ledger. Namespace and run identities are independent of nodes.
package workflowstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ledger"
)

const headKind = "workflow.head"

type Store struct {
	book *ledger.Ledger
	now  func() time.Time
}

var _ workflow.Store = (*Store)(nil)

// New borrows a generation-scoped ledger; it neither creates another database
// nor closes the supplied ledger. The caller retains its activation lifecycle.
func New(book *ledger.Ledger) *Store { return &Store{book: book, now: time.Now} }

func (s *Store) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.book == nil {
		return errors.New("workflowstore: ledger is unavailable")
	}
	return nil
}
func (s *Store) update(ctx context.Context, change func(*ledger.Tx) error) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.book.Update(ctx, change)
}

func scope(kind, id string) string {
	return kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}
func sequenceKey(version int64) string { return fmt.Sprintf("%020d", version) }

func readBinding(tx *ledger.Tx, kind, id string, result any) (bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), result)
}

func head(tx *ledger.Tx, runID string) (workflow.CheckpointRecord, error) {
	var record workflow.CheckpointRecord
	found, err := readBinding(tx, headKind, runID, &record)
	if err != nil {
		return record, err
	}
	if !found {
		return record, workflow.ErrCheckpointNotFound
	}
	if record.RunID != runID || record.Version <= 0 {
		return record, errors.New("workflowstore: invalid checkpoint head")
	}
	return record, nil
}

func saveVersion(tx *ledger.Tx, record workflow.CheckpointRecord) error {
	record.LeaseDuration = 0
	if err := tx.PutBinding(scope("workflow.checkpoint", record.RunID), sequenceKey(record.Version), record); err != nil {
		return err
	}
	record.Payload = nil
	return tx.PutBinding(headKind, record.RunID, record)
}

func (s *Store) Create(ctx context.Context, record workflow.CheckpointRecord) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if record.Version != 1 || record.Status != workflow.CheckpointRunning {
		return fmt.Errorf("%w: new checkpoint must run at version one", workflow.ErrInvalidCheckpoint)
	}
	if err := validateCheckpoint(record); err != nil {
		return err
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		if _, err := head(tx, record.RunID); err == nil {
			return workflow.ErrCheckpointExists
		} else if !errors.Is(err, workflow.ErrCheckpointNotFound) {
			return err
		}
		if record.LeaseDuration > 0 {
			record.LeaseExpiresAt = s.now().Add(record.LeaseDuration)
		}
		return saveVersion(tx, record)
	})
}

func (s *Store) Load(ctx context.Context, runID string) (workflow.CheckpointRecord, error) {
	if err := s.ready(ctx); err != nil {
		return workflow.CheckpointRecord{}, err
	}
	if !validID(runID) {
		return workflow.CheckpointRecord{}, fmt.Errorf("%w: invalid run id", workflow.ErrInvalidCheckpoint)
	}
	var result workflow.CheckpointRecord
	err := s.update(ctx, func(tx *ledger.Tx) error {
		current, err := head(tx, runID)
		if err != nil {
			return err
		}
		var historical workflow.CheckpointRecord
		found, err := readBinding(tx, scope("workflow.checkpoint", runID), sequenceKey(current.Version), &historical)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("workflowstore: checkpoint history is missing")
		}
		current.Payload = historical.Payload
		result = current
		return nil
	})
	if err != nil {
		return workflow.CheckpointRecord{}, err
	}
	return result, nil
}

func (s *Store) Claim(ctx context.Context, candidate workflow.CheckpointRecord, version int64) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if version <= 0 || version == maxSequence || candidate.Version != version || candidate.Status != workflow.CheckpointRunning || candidate.OwnerID == "" || candidate.ClaimSequence <= 0 || (candidate.LeaseDuration == 0 && candidate.LeaseExpiresAt.IsZero()) {
		return fmt.Errorf("%w: invalid checkpoint claim", workflow.ErrInvalidCheckpoint)
	}
	if err := validateCheckpoint(candidate); err != nil {
		return err
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		current, err := head(tx, candidate.RunID)
		if err != nil {
			return err
		}
		if current.Version != version || (current.Status != workflow.CheckpointRunning && current.Status != workflow.CheckpointInterrupted) {
			return workflow.ErrCheckpointConflict
		}
		if !sameIdentity(current, candidate) {
			return workflow.ErrCheckpointMismatch
		}
		now := s.now()
		if candidate.LeaseDuration > 0 {
			candidate.LeaseExpiresAt = now.Add(candidate.LeaseDuration)
		}
		if !candidate.LeaseExpiresAt.After(now) {
			return fmt.Errorf("%w: claim lease must expire in the future", workflow.ErrInvalidCheckpoint)
		}
		if current.LeaseExpiresAt.After(now) || current.ClaimSequence == maxSequence || candidate.ClaimSequence != current.ClaimSequence+1 {
			return workflow.ErrCheckpointConflict
		}
		candidate.Version = version + 1
		return saveVersion(tx, candidate)
	})
}

func (s *Store) RenewLease(ctx context.Context, lease workflow.CheckpointLease) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if !validID(lease.RunID) || lease.OwnerID == "" || lease.ClaimSequence <= 0 || lease.Duration < 0 || (lease.Duration == 0 && lease.ExpiresAt.IsZero()) {
		return fmt.Errorf("%w: invalid lease renewal", workflow.ErrInvalidCheckpoint)
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		current, err := head(tx, lease.RunID)
		if errors.Is(err, workflow.ErrCheckpointNotFound) {
			return workflow.ErrCheckpointLeaseLost
		}
		if err != nil {
			return err
		}
		now := s.now()
		if current.Status != workflow.CheckpointRunning || current.OwnerID != lease.OwnerID || current.ClaimSequence != lease.ClaimSequence || !current.LeaseExpiresAt.After(now) {
			return workflow.ErrCheckpointLeaseLost
		}
		expires := lease.ExpiresAt
		if lease.Duration > 0 {
			expires = now.Add(lease.Duration)
		}
		if !expires.After(now) {
			return fmt.Errorf("%w: renewed lease must expire in the future", workflow.ErrInvalidCheckpoint)
		}
		if !expires.After(current.LeaseExpiresAt) {
			return nil
		}
		current.LeaseExpiresAt = expires
		return tx.PutBinding(headKind, current.RunID, current)
	})
}

func (s *Store) Save(ctx context.Context, record workflow.CheckpointRecord, version int64) error {
	return s.write(ctx, record, version, false)
}
func (s *Store) Finish(ctx context.Context, record workflow.CheckpointRecord, version int64) error {
	return s.write(ctx, record, version, true)
}

func (s *Store) write(ctx context.Context, record workflow.CheckpointRecord, version int64, terminal bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if version <= 0 || version == maxSequence || record.Version != version {
		return fmt.Errorf("%w: invalid checkpoint version", workflow.ErrCheckpointConflict)
	}
	if terminal != isTerminal(record.Status) {
		return fmt.Errorf("%w: status does not match save operation", workflow.ErrInvalidCheckpoint)
	}
	if err := validateCheckpoint(record); err != nil {
		return err
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		current, err := head(tx, record.RunID)
		if err != nil {
			return err
		}
		if !sameIdentity(current, record) {
			return workflow.ErrCheckpointMismatch
		}
		now := s.now()
		if record.ClaimSequence != current.ClaimSequence || (record.OwnerID != "" && record.OwnerID != current.OwnerID) || (current.OwnerID != "" && !current.LeaseExpiresAt.After(now)) {
			return workflow.ErrCheckpointLeaseLost
		}
		if isTerminal(current.Status) || current.Version != version {
			return workflow.ErrCheckpointConflict
		}
		if record.OwnerID == "" {
			record.LeaseExpiresAt = time.Time{}
		} else {
			record.LeaseExpiresAt = current.LeaseExpiresAt
			if record.LeaseDuration > 0 {
				if later := now.Add(record.LeaseDuration); later.After(record.LeaseExpiresAt) {
					record.LeaseExpiresAt = later
				}
			}
		}
		record.Version = version + 1
		return saveVersion(tx, record)
	})
}

func (s *Store) ListCheckpoints(ctx context.Context, request workflow.CheckpointHistoryRequest) ([]workflow.CheckpointRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validID(request.RunID) || request.AfterVersion < 0 || request.Limit < 0 {
		return nil, fmt.Errorf("%w: invalid history query", workflow.ErrInvalidCheckpoint)
	}
	records := []workflow.CheckpointRecord{}
	err := s.update(ctx, func(tx *ledger.Tx) error {
		latest, err := head(tx, request.RunID)
		if err != nil {
			return err
		}
		if request.AfterVersion >= latest.Version {
			return nil
		}
		for version := request.AfterVersion + 1; version <= latest.Version; version++ {
			var record workflow.CheckpointRecord
			found, err := readBinding(tx, scope("workflow.checkpoint", request.RunID), sequenceKey(version), &record)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("workflowstore: checkpoint history is missing")
			}
			records = append(records, record)
			if (request.Limit > 0 && len(records) == request.Limit) || version == maxSequence {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

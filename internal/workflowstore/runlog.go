package workflowstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/gopact/runlog"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ledger"
)

const logDataKind = "workflow.event"
const logIndexKind = "workflow.log-index"

type logPointer struct {
	Ordinal   int64
	Sequence  int64
	RunID     string
	SessionID string
}

func validateEvent(record runlog.Record) ([]byte, error) {
	if !validID(record.RunID) || !validID(record.SessionID) || record.Sequence <= 0 || record.EventType == "" || record.Source == "" || record.Timestamp.IsZero() || !validLineage(record.SourceRunID, record.SourceEventSeq, record.SourceRevisionID) {
		return nil, fmt.Errorf("%w: incomplete event identity", runlog.ErrInvalidRecord)
	}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > maxPayload {
		return nil, fmt.Errorf("%w: invalid event payload", runlog.ErrInvalidRecord)
	}
	return encoded, nil
}

func (s *Store) Append(ctx context.Context, record runlog.Record) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	encoded, err := validateEvent(record)
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *ledger.Tx) error { return appendEvent(tx, record, encoded) })
}

func (s *Store) AppendFenced(ctx context.Context, record runlog.Record, fence runlog.Fence) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if fence.OwnerID == "" || fence.ClaimSequence <= 0 {
		return fmt.Errorf("%w: invalid journal fence", workflow.ErrInvalidCheckpoint)
	}
	encoded, err := validateEvent(record)
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		current, err := head(tx, record.RunID)
		if errors.Is(err, workflow.ErrCheckpointNotFound) {
			return workflow.ErrCheckpointLeaseLost
		}
		if err != nil {
			return err
		}
		if current.Status != workflow.CheckpointRunning || current.OwnerID != fence.OwnerID || current.ClaimSequence != fence.ClaimSequence || !current.LeaseExpiresAt.After(s.now()) {
			return workflow.ErrCheckpointLeaseLost
		}
		return appendEvent(tx, record, encoded)
	})
}

func appendEvent(tx *ledger.Tx, record runlog.Record, encoded []byte) error {
	var existing int64
	found, err := readBinding(tx, scope("workflow.event-key", record.RunID), sequenceKey(record.Sequence), &existing)
	if err != nil {
		return err
	}
	if found {
		var stored json.RawMessage
		found, err := readBinding(tx, logDataKind, sequenceKey(existing), &stored)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("workflowstore: event index references missing event")
		}
		if bytes.Equal(stored, encoded) {
			return nil
		}
		return runlog.ErrConflict
	}
	var ordinal int64
	if _, err := readBinding(tx, "workflow.sequence", "events", &ordinal); err != nil {
		return err
	}
	if ordinal == maxSequence {
		return fmt.Errorf("%w: journal sequence exhausted", runlog.ErrInvalidRecord)
	}
	ordinal++
	if err := tx.PutBinding("workflow.sequence", "events", ordinal); err != nil {
		return err
	}
	if err := tx.PutBinding(logDataKind, sequenceKey(ordinal), json.RawMessage(encoded)); err != nil {
		return err
	}
	if err := tx.PutBinding(scope("workflow.event-key", record.RunID), sequenceKey(record.Sequence), ordinal); err != nil {
		return err
	}
	pointer := logPointer{Ordinal: ordinal, Sequence: record.Sequence, RunID: record.RunID, SessionID: record.SessionID}
	for _, kind := range []string{logIndexKind, scope("workflow.run-index", record.RunID), scope("workflow.session-index", record.SessionID)} {
		if err := tx.PutBinding(kind, sequenceKey(ordinal), pointer); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) List(ctx context.Context, query runlog.Query) ([]runlog.Record, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if query.After < 0 || query.Limit < 0 || (query.RunID != "" && !validID(query.RunID)) || (query.SessionID != "" && !validID(query.SessionID)) || (query.SessionID != "" && query.RunID == "" && query.After != 0) {
		return nil, fmt.Errorf("%w: invalid log query", runlog.ErrInvalidQuery)
	}
	result := []runlog.Record{}
	err := s.update(ctx, func(tx *ledger.Tx) error {
		kind := logIndexKind
		if query.RunID != "" {
			kind = scope("workflow.run-index", query.RunID)
		} else if query.SessionID != "" {
			kind = scope("workflow.session-index", query.SessionID)
		}
		index, err := tx.Bindings(kind)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(index))
		for key := range index {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var pointer logPointer
			if err := json.Unmarshal(index[key], &pointer); err != nil {
				return err
			}
			if pointer.Sequence <= query.After || (query.RunID != "" && pointer.RunID != query.RunID) || (query.SessionID != "" && pointer.SessionID != query.SessionID) {
				continue
			}
			var record runlog.Record
			found, err := readBinding(tx, logDataKind, sequenceKey(pointer.Ordinal), &record)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("workflowstore: event index references missing event")
			}
			result = append(result, record)
			if query.Limit > 0 && len(result) == query.Limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

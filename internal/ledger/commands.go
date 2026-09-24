package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CommandRecord is a command's durable receipt. A nil FinishedAt is an
// unresolved dispatch, not permission to run its callback again.
type CommandRecord struct {
	ID, Kind, Actor string
	ReceivedAt      time.Time
	FinishedAt      *time.Time
	Result          json.RawMessage
	Error           string
}

// RecordCommand accepts an already known result in one transaction. It is
// deliberately not a callback API: no external work may depend on acceptance
// before this returns. Reusing a key requires identical kind, actor and bytes.
func (l *Ledger) RecordCommand(ctx context.Context, id, kind, actor string, result json.RawMessage) error {
	if id == "" || kind == "" || !json.Valid(result) {
		return errors.New("ledger: command acceptance requires an id, kind and JSON result")
	}
	return l.Update(ctx, func(tx *Tx) error {
		var oldKind, oldActor string
		var finished, oldResult, oldError sql.NullString
		err := tx.QueryRow(`SELECT kind, actor, finished_at, result, error FROM commands WHERE id = ?`, id).
			Scan(&oldKind, &oldActor, &finished, &oldResult, &oldError)
		if errors.Is(err, sql.ErrNoRows) {
			now := l.now().UTC().Format(time.RFC3339Nano)
			_, err = tx.Exec(`INSERT INTO commands(id, kind, actor, received_at, finished_at, result, error) VALUES (?, ?, ?, ?, ?, ?, '')`,
				id, kind, actor, now, now, string(result))
			return err
		}
		if err != nil {
			return err
		}
		if oldKind != kind || oldActor != actor {
			return fmt.Errorf("%w: command %s has another identity", ErrConflict, id)
		}
		if !finished.Valid {
			return ErrInFlight
		}
		if oldError.String != "" || !bytes.Equal([]byte(oldResult.String), result) {
			return fmt.Errorf("%w: command %s has another result", ErrConflict, id)
		}
		return nil
	})
}

// Commands reads the receipts owned by one command kind, including unresolved
// dispatches. It never reserves, completes or retries a command.
func (l *Ledger) Commands(ctx context.Context, kind string) ([]CommandRecord, error) {
	if kind == "" {
		return nil, errors.New("ledger: a command kind is required")
	}
	rows, err := l.reads.QueryContext(ctx, `SELECT id, kind, actor, received_at, finished_at, result, error FROM commands WHERE kind = ? ORDER BY received_at, id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CommandRecord
	for rows.Next() {
		var r CommandRecord
		var received string
		var finished, raw, cmdErr sql.NullString
		if err := rows.Scan(&r.ID, &r.Kind, &r.Actor, &received, &finished, &raw, &cmdErr); err != nil {
			return nil, err
		}
		r.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
		if err != nil {
			return nil, fmt.Errorf("command %s received_at: %w", r.ID, err)
		}
		if finished.Valid {
			at, err := time.Parse(time.RFC3339Nano, finished.String)
			if err != nil {
				return nil, fmt.Errorf("command %s finished_at: %w", r.ID, err)
			}
			r.FinishedAt = &at
		}
		r.Result, r.Error = json.RawMessage(raw.String), cmdErr.String
		result = append(result, r)
	}
	return result, rows.Err()
}

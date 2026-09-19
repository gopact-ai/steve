package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CommandReceipt reads one immutable identity's dispatch evidence. Missing,
// unfinished and finished receipts are distinct; none starts or retries work.
func (l *Ledger) CommandReceipt(ctx context.Context, id string) (CommandRecord, bool, error) {
	if id == "" {
		return CommandRecord{}, false, errors.New("ledger: command id is required")
	}
	var r CommandRecord
	var received string
	var finished, raw, cmdErr sql.NullString
	err := l.db.QueryRowContext(ctx, `SELECT id, kind, actor, received_at, finished_at, result, error FROM commands WHERE id = ?`, id).
		Scan(&r.ID, &r.Kind, &r.Actor, &received, &finished, &raw, &cmdErr)
	if errors.Is(err, sql.ErrNoRows) {
		return CommandRecord{}, false, nil
	}
	if err != nil {
		return CommandRecord{}, false, err
	}
	r.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
	if err != nil {
		return CommandRecord{}, false, fmt.Errorf("command %s received_at: %w", id, err)
	}
	if finished.Valid {
		if !cmdErr.Valid {
			return CommandRecord{}, false, fmt.Errorf("command %s finished without a durable outcome", id)
		}
		at, err := time.Parse(time.RFC3339Nano, finished.String)
		if err != nil {
			return CommandRecord{}, false, fmt.Errorf("command %s finished_at: %w", id, err)
		}
		r.FinishedAt = &at
	}
	r.Result, r.Error = json.RawMessage(raw.String), cmdErr.String
	return r, true, nil
}

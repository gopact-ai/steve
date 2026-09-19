package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EventPosition is an exclusive chronological boundary, not an insertion
// sequence boundary. Seq breaks ties between events at the same instant.
type EventPosition struct {
	At  time.Time
	Seq int64
}

// Ledger writers store UTC RFC3339Nano. Removing Z makes the variable-width
// fractional seconds sort correctly without losing nanosecond precision:
// "00" < "00.001" < "00.01". SQLite date functions round away that precision.
const historyTimeKey = `rtrim(at, 'Z')`

func init() {
	MustRegisterReadIndex("events_history_time_seq", `CREATE INDEX IF NOT EXISTS events_history_time_seq ON events(`+historyTimeKey+`, seq)`)
}

func historyEventQuery(boundary string) string {
	query := `SELECT seq, operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at
		FROM events INDEXED BY events_history_time_seq WHERE seq <= ?`
	query += boundary
	if boundary == historySameTime {
		return query + ` ORDER BY seq DESC LIMIT ?`
	}
	return query + ` ORDER BY ` + historyTimeKey + ` DESC, seq DESC LIMIT ?`
}

const historySameTime = ` AND ` + historyTimeKey + ` = ? AND seq < ?`
const historyOlder = ` AND ` + historyTimeKey + ` < ?`

// HistoryEvents reads at most limit events in chronological order. A nil
// through starts a traversal and captures the current maximum sequence;
// subsequent pages reuse that fence, excluding even backdated new events.
// Only the requested page is decoded, never the full journal.
func (l *Ledger) HistoryEvents(ctx context.Context, before *EventPosition, through *int64, limit int) ([]Event, int64, error) {
	if limit < 1 || limit > 201 {
		return nil, 0, fmt.Errorf("ledger: history limit must be between 1 and 201")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var fence int64
	if through != nil {
		fence = *through
	} else if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&fence); err != nil {
		return nil, 0, err
	}
	if before == nil {
		events, err := readHistoryEvents(ctx, tx, "", fence, limit)
		return events, fence, err
	}
	at := strings.TrimSuffix(before.At.UTC().Format(time.RFC3339Nano), "Z")
	// SQLite cannot seek a row-value comparison whose first term is an
	// expression. Two disjoint seeks implement the same strict tuple order,
	// including arbitrarily many equal timestamps, without an index scan.
	out, err := readHistoryEvents(ctx, tx, historySameTime, fence, at, before.Seq, limit)
	if err != nil || len(out) == limit {
		return out, fence, err
	}
	older, err := readHistoryEvents(ctx, tx, historyOlder, fence, at, limit-len(out))
	return append(out, older...), fence, err
}

func readHistoryEvents(ctx context.Context, tx *sql.Tx, boundary string, args ...any) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, historyEventQuery(boundary), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var fencings, at string
		var effects []byte
		if err := rows.Scan(&ev.Seq, &ev.OperationID, &ev.Revision, &ev.Incarnation, &ev.From, &ev.To, &ev.Actor, &fencings, &effects, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fencings), &ev.Fencings); err != nil {
			return nil, fmt.Errorf("ledger: event %d fencings: %w", ev.Seq, err)
		}
		ev.Effects = effects
		if ev.At, err = stamp(at); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

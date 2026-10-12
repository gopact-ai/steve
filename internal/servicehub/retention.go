package servicehub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/interaction"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/work"
)

type RetentionPolicy struct {
	After time.Duration
	Batch int
}
type Compaction = durable.Compaction

type retentionState struct {
	mu         sync.Mutex
	cursors    map[string]string
	next       time.Time
	coldCursor string
}

// Compact moves bounded cold terminal payloads into lossless gzip records in
// the same ledger. Identity, ingress and authorization proofs remain hot.
// Unknown and unfinished records are never selected.
func (s *Services) Compact(ctx context.Context) (out Compaction, err error) {
	s.retention.mu.Lock()
	defer s.retention.mu.Unlock()
	if s.retention.cursors == nil {
		s.retention.cursors = map[string]string{}
	}
	after := s.Retention.After
	if after <= 0 {
		after = 7 * 24 * time.Hour
	}
	batch := s.Retention.Batch
	if batch <= 0 {
		batch = 32
	}
	if batch > 200 {
		batch = 200
	}
	cutoff := time.Now().UTC().Add(-after).Format(time.RFC3339Nano)
	for _, candidate := range []struct {
		kind, stateExpr string
		states          []string
	}{
		{events.EventKind, "", nil},
		{events.DeliveryKind, durable.StateExpression, []string{events.StateDone, events.StateFailed, events.StateCancelled}},
		// A refusal before admission has no exchange or Outcome, but its
		// failed receipt is just as terminal as a completed execution.
		{work.RecordKind, "CASE WHEN (" + durable.StateExpression + ")='failed' THEN 'failed' ELSE (" + durable.OutcomeExpression + ") END", []string{"done", "failed", "cancelled"}},
		{work.InputKind, durable.StateExpression, []string{work.InputDone}},
		{interaction.RecordKind, durable.StateExpression, []string{"answered", "interrupted"}},
		{clientcap.CallKind, durable.StateExpression, []string{clientcap.StateDone, clientcap.StateFailed, clientcap.StateCancelled}},
	} {
		var ids []string
		err = s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
			query := `SELECT id FROM bindings WHERE kind=? AND id>? AND updated_at<? AND length(data)>=512 AND json_extract(data,'$._steve_cold_record') IS NULL`
			args := []any{candidate.kind, s.retention.cursors[candidate.kind], cutoff}
			if candidate.stateExpr != "" {
				marks := make([]string, len(candidate.states))
				for i, state := range candidate.states {
					marks[i] = "?"
					args = append(args, state)
				}
				query += ` AND (` + candidate.stateExpr + `) IN (` + strings.Join(marks, ",") + `)`
			}
			query += ` ORDER BY id LIMIT ?`
			args = append(args, batch)
			rows, e := tx.Query(query, args...)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if e := rows.Scan(&id); e != nil {
					return e
				}
				ids = append(ids, id)
			}
			return rows.Err()
		})
		if err != nil {
			return out, err
		}
		for _, id := range ids {
			out.Scanned++
			err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
				var rawKind string
				if err := tx.QueryRow(`SELECT kind FROM bindings WHERE kind=? AND id=? AND updated_at<? AND json_extract(data,'$._steve_cold_record') IS NULL`, candidate.kind, id, cutoff).Scan(&rawKind); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return nil
					}
					return err
				}
				if candidate.kind == events.EventKind {
					ready, err := coldEventTx(tx, id)
					if err != nil || !ready {
						return err
					}
				} else {
					var state string
					if err := tx.QueryRow(`SELECT coalesce((`+candidate.stateExpr+`),'') FROM bindings WHERE kind=? AND id=?`, candidate.kind, id).Scan(&state); err != nil {
						return err
					}
					terminal := false
					for _, allowed := range candidate.states {
						terminal = terminal || state == allowed
					}
					if !terminal {
						return nil
					}
				}
				saved, err := durable.ArchiveTx(tx, candidate.kind, id)
				if err == nil && saved > 0 {
					out.Archived++
					out.SavedBytes += saved
				}
				return err
			})
			if err != nil {
				return out, err
			}
			s.retention.cursors[candidate.kind] = id
		}
		if len(ids) < batch {
			s.retention.cursors[candidate.kind] = ""
		}
	}
	s.retention.coldCursor, err = durable.SweepCold(ctx, s.Book, s.retention.coldCursor, batch)
	return out, err
}

func coldEventTx(tx *ledger.Tx, id string) (bool, error) {
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind=? AND id=?`, events.OutboxKind, id).Scan(&count); err != nil || count > 0 {
		return false, err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind=? AND (`+durable.EventIDExpression+`)=? AND (`+durable.StateExpression+`) IN ('pending','claimed','accepted','unknown')`, events.DeliveryKind, id).Scan(&count); err != nil || count > 0 {
		return false, err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM bindings AS i JOIN bindings AS q ON q.kind=? AND q.id=i.id WHERE i.kind=? AND json_extract(i.data,'$.event_id')=?`, events.InboxOpenKind, events.InboxKind, id).Scan(&count); err != nil || count > 0 {
		return false, err
	}
	return true, nil
}

func (s *Services) maintain(ctx context.Context) error {
	s.retention.mu.Lock()
	if s.retention.next.After(time.Now()) {
		s.retention.mu.Unlock()
		return nil
	}
	s.retention.next = time.Now().Add(time.Minute)
	s.retention.mu.Unlock()
	_, err := s.Compact(ctx)
	if err != nil {
		return fmt.Errorf("service history compaction: %w", err)
	}
	return nil
}

func init() {
	ledger.MustRegisterReadIndex("service_event_reference_v1", `CREATE INDEX IF NOT EXISTS service_event_reference_v1 ON bindings(kind,`+durable.EventIDExpression+`,id)`)
	ledger.MustRegisterReadIndex("service_retention_v1", `CREATE INDEX IF NOT EXISTS service_retention_v1 ON bindings(kind,updated_at,id)`)
}

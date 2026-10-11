package work

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Accepted is queue admission, not completion. Keep the original exchange's
// final receipt before releasing backlog capacity or compressing history.
const unfinishedPredicate = `(json_extract(data,'$.state')='pending' OR (json_extract(data,'$.state')='accepted' AND coalesce(json_extract(data,'$.outcome.state'),'') NOT IN ('done','failed','cancelled')))`

func init() {
	ledger.MustRegisterReadIndex("bindings_work_unfinished", `CREATE INDEX IF NOT EXISTS bindings_work_unfinished ON bindings(id) WHERE kind='service-work' AND `+unfinishedPredicate)
	ledger.MustRegisterReadIndex("bindings_work_unfinished_source", `CREATE INDEX IF NOT EXISTS bindings_work_unfinished_source ON bindings(json_extract(data,'$.grant_id'),id) WHERE kind='service-work' AND `+unfinishedPredicate)
}
func terminalResult(state string) bool {
	return state == "done" || state == "failed" || state == "cancelled"
}
func (s *Service) checkCapacityTx(tx *ledger.Tx, source string) error {
	limits := s.Limits.Effective()
	// Predicate indexes cover pre-upgrade records while the bounded worker observes final outcomes. Retries already returned their old receipt.
	var total, own int
	if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind=? AND `+unfinishedPredicate, RecordKind).Scan(&total); err != nil {
		return err
	}
	sourceQuery := `SELECT count(*) FROM bindings WHERE kind=? AND ` + unfinishedPredicate + ` AND json_extract(data,'$.grant_id')=?`
	args := []any{RecordKind, source}
	if source == "" {
		sourceQuery = `SELECT count(*) FROM bindings WHERE kind=? AND ` + unfinishedPredicate + ` AND json_extract(data,'$.grant_id') IS NULL`
		args = []any{RecordKind}
	}
	if err := tx.QueryRow(sourceQuery, args...).Scan(&own); err != nil {
		return err
	}
	if total >= limits.Total || own >= limits.PerSource {
		return fmt.Errorf("%w: work unfinished limit", durable.ErrCapacity)
	}
	return nil
}
func (s *Service) sealResult(ctx context.Context, record Record, result Result) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		current, found, err := durable.Get[Record](tx, RecordKind, record.ID)
		if err != nil {
			return err
		}
		if !found || current.Exchange != record.Exchange {
			return ledger.ErrConflict
		}
		if current.Outcome != nil {
			return nil
		}
		current.Outcome = &result
		if err := tx.PutBinding(RecordKind, record.ID, current); err != nil {
			return err
		}
		return nil
	})
}

// Sweep observes the original accepted exchange. It never resubmits an input
// or turns a missing/uncertain outcome into a new attempt.
func (s *Service) Sweep(ctx context.Context) error {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	var ids []string
	err := s.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, err := tx.Query(`SELECT id FROM bindings WHERE kind=? AND `+unfinishedPredicate+` AND id>? ORDER BY id LIMIT 33`, RecordKind, s.sweepAfter)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	s.sweepAfter = ""
	if len(ids) > 32 {
		ids = ids[:32]
		s.sweepAfter = ids[31]
	}
	var failures []error
	for _, id := range ids {
		_, _, err := s.Status(ctx, identity.Principal{Owner: true}, id)
		if err != nil && !errors.Is(err, ledger.ErrConflict) && ctx.Err() == nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// BacklogTx derives occupancy from business facts, including pre-upgrade records.
func BacklogTx(r ledger.Reader) (out durable.QueueStats, err error) {
	err = r.QueryRow(`SELECT count(*),coalesce(min(json_extract(data,'$.at')),'') FROM bindings WHERE kind=? AND `+unfinishedPredicate, RecordKind).Scan(&out.Pending, &out.Oldest)
	return
}

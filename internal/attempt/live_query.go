package attempt

import (
	"context"
	"encoding/json"

	"github.com/gopact-ai/steve/internal/ledger"
)

// The partial index contains active, unconfirmed, and malformed attempts.
// Settled history never participates in a live query; malformed data still
// fails closed rather than making the workspace appear safe to write.
const liveAttemptQuery = `SELECT id, state, revision, data FROM operations INDEXED BY operations_live_attempts WHERE kind = 'attempt' AND (state IN ('leased','prepared','running','snapshotted','published','durable','verifying','bind-ready') OR CASE WHEN json_valid(data) THEN COALESCE(json_extract(data, '$.unsettled'), 0) ELSE 1 END != 0) ORDER BY updated_at DESC`

func (s *Service) liveRecords(ctx context.Context) ([]Record, error) {
	rows, err := s.l.DB().QueryContext(ctx, liveAttemptQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var op ledger.Operation
		var raw string
		if err := rows.Scan(&op.ID, &op.State, &op.Revision, &raw); err != nil {
			return nil, err
		}
		op.Data = json.RawMessage(raw)
		r, err := decode(op)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

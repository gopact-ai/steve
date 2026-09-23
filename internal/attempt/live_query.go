package attempt

import (
	"context"
	"database/sql/driver"
	"encoding/json"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

// The partial index contains active, unconfirmed, and malformed attempts.
// Settled history never participates in a live query; malformed data still
// fails closed rather than making the workspace appear safe to write.
const liveAttemptPredicate = `kind = 'attempt' AND steve_attempt_live_v2(state, data) != 0`
const liveAttemptQuery = `SELECT id, state, revision, data FROM operations INDEXED BY operations_live_attempts WHERE ` + liveAttemptPredicate + ` ORDER BY updated_at DESC`

func init() {
	// JSON path semantics differ from encoding/json for duplicate keys,
	// case-insensitive fields and nested type errors. Only the schema owner
	// can classify a settled record. Invalid payloads must remain candidates
	// so the normal reader reports them instead of hiding a possible writer.
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_live_v2", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		state, ok := args[0].(string)
		if !ok || !State(state).Terminal() {
			return int64(1), nil
		}
		var raw []byte
		switch data := args[1].(type) {
		case string:
			raw = []byte(data)
		case []byte:
			raw = data
		default:
			return int64(1), nil
		}
		record, err := decode(ledger.Operation{State: state, Data: raw})
		if err != nil || record.Unsettled {
			return int64(1), nil
		}
		return int64(0), nil
	})
	ledger.MustRegisterReadIndex("operations_live_attempts", `CREATE INDEX IF NOT EXISTS operations_live_attempts ON operations(updated_at DESC) WHERE `+liveAttemptPredicate)
}

func (s *Service) liveRecords(ctx context.Context) ([]Record, error) {
	return s.indexedRecords(ctx, liveAttemptQuery)
}

// indexedRecords decodes every row of a partial-index query in full, so a
// malformed payload the index kept fails the read.
func (s *Service) indexedRecords(ctx context.Context, query string) ([]Record, error) {
	rows, err := s.l.DB().QueryContext(ctx, query)
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

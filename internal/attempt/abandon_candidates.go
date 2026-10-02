package attempt

import (
	"context"
	"database/sql/driver"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const abandonPendingPredicate = `kind='attempt' AND steve_attempt_abandon_pending_v1(id,state,data) != 0`
const abandonPendingQuery = `SELECT id,state,revision,data FROM operations INDEXED BY operations_attempt_abandon_pending WHERE ` + abandonPendingPredicate + ` ORDER BY updated_at DESC`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_abandon_pending_v1", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		state, ok := args[1].(string)
		if !ok {
			return int64(1), nil
		}
		var raw []byte
		switch value := args[2].(type) {
		case string:
			raw = []byte(value)
		case []byte:
			raw = value
		default:
			return int64(1), nil
		}
		r, err := decodeAbandonProjection(ledger.Operation{ID: id, State: state, Data: raw})
		if err != nil || abandonProjectionOwed(r) {
			return int64(1), nil
		}
		return int64(0), nil
	})
	ledger.MustRegisterReadIndex("operations_attempt_abandon_pending", `CREATE INDEX IF NOT EXISTS operations_attempt_abandon_pending ON operations(updated_at DESC) WHERE `+abandonPendingPredicate)
}

func abandonProjectionOwed(r Record) bool {
	return r.Abandoned != nil && (r.Abandoned.ProjectedAt.IsZero() || r.Abandoned.DeliveryDoneAt.IsZero())
}

func decodeAbandonProjection(op ledger.Operation) (Record, error) {
	r, err := decodeIdentityRecord(op)
	if err == nil && r.Abandoned != nil && !r.Abandoned.DeliveryDoneAt.IsZero() && !r.Abandoned.DeliveryResult.valid() {
		return Record{}, errors.New("abandonment has invalid delivery completion evidence")
	}
	return r, err
}

// AbandonProjections retains only unfinished session or receiver obligations.
// Physical confirmation and a completed session projection do not retire a
// failed reply. Completed history is excluded by this owner-decoded index.
func (s *Service) AbandonProjections(ctx context.Context) ([]Record, error) {
	return s.indexedRecords(ctx, abandonPendingQuery, decodeAbandonProjection)
}

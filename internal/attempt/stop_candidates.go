package attempt

import (
	"context"
	"database/sql/driver"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

// The partial index holds exactly the records TaskStopOwed accepts, plus
// every payload the owner decoder refuses, so the reader reports a malformed
// row instead of hiding a possible writer. v2 stopped accepting confirmed
// stops whose accounting projection is recorded. Opening a ledger replaces
// an index still built on v1: its stored definition no longer matches, so
// the ledger drops and rebuilds it, and v1 itself is not registered.
const stopCandidatePredicate = `kind = 'attempt' AND steve_attempt_stop_candidate_v2(id, state, data) != 0`
const stopCandidateQuery = `SELECT id, state, revision, data FROM operations INDEXED BY operations_attempt_stop_candidates WHERE ` + stopCandidatePredicate + ` ORDER BY updated_at DESC`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_stop_candidate_v2", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		state, ok := args[1].(string)
		if !ok {
			return int64(1), nil
		}
		var raw []byte
		switch data := args[2].(type) {
		case string:
			raw = []byte(data)
		case []byte:
			raw = data
		default:
			return int64(1), nil
		}
		r, err := decodeIdentityRecord(ledger.Operation{ID: id, State: state, Data: raw})
		if err != nil || TaskStopOwed(r) {
			return int64(1), nil
		}
		return int64(0), nil
	})
	ledger.MustRegisterReadIndex("operations_attempt_stop_candidates", `CREATE INDEX IF NOT EXISTS operations_attempt_stop_candidates ON operations(updated_at DESC) WHERE `+stopCandidatePredicate)
}

// TaskStopOwed reports whether a durable task stop pass acts on r: a
// node-owned execution whose native stop is not yet settled, or a confirmed
// task stop whose accounting projection is not yet recorded. Both the stop
// pass and the candidate index use it. Changing what it accepts changes an
// indexed expression: rename steve_attempt_stop_candidate_v2 to a new
// version when it does.
func TaskStopOwed(r Record) bool {
	if !nodeOwnedStop(r) {
		return false
	}
	return !taskStopAlreadySettled(r) || TaskStopConfirmed(r) && !r.StopProjected
}

// StopCandidates is every attempt TaskStopOwed accepts, most recently
// updated first. Its cost follows that set, not the settled history: stops
// whose native session is unsettled, plus confirmed task stops until
// MarkStopProjected records their accounting.
func (s *Service) StopCandidates(ctx context.Context) ([]Record, error) {
	return s.indexedRecords(ctx, stopCandidateQuery, decodeIdentityRecord)
}

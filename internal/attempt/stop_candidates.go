package attempt

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

// The partial index holds exactly the records TaskStopOwed accepts, plus
// every payload the owner decoder refuses, so the reader reports a malformed
// row instead of hiding a possible writer.
const stopCandidatePredicate = `kind = 'attempt' AND steve_attempt_stop_candidate_v1(id, state, data) != 0`
const stopCandidateQuery = `SELECT id, state, revision, data FROM operations INDEXED BY operations_attempt_stop_candidates WHERE ` + stopCandidatePredicate + ` ORDER BY updated_at DESC`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_stop_candidate_v1", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
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
// node-owned execution whose native stop is not yet settled, or a settled
// one whose own task stop committed and still needs its accounting and
// in-memory owner reconciled. Both the stop pass and the candidate index
// use it. Changing what it accepts changes an indexed expression: rename
// steve_attempt_stop_candidate_v1 to a new version when it does.
func TaskStopOwed(r Record) bool {
	if r.State == Superseded || (!strings.HasPrefix(r.Session, "ns_") && !PendingSessionOpen(r)) || r.Node == "" || r.Execution == nil {
		return false
	}
	settled := r.State.Terminal() && !r.Unsettled && r.SessionSettled != nil && *r.SessionSettled
	return !settled || r.StopEvidence == "task-stop/"+r.ID
}

// StopCandidates is every attempt TaskStopOwed accepts, most recently
// updated first. Its cost follows that set, not the settled history.
func (s *Service) StopCandidates(ctx context.Context) ([]Record, error) {
	return s.indexedRecords(ctx, stopCandidateQuery, decodeIdentityRecord)
}

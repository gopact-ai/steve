package attempt

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

// The partial index holds the live set plus settled executions that a durable
// task stop pass still acts on: a native stop that is not yet confirmed, or a
// confirmed one whose in-memory owner may still be waiting for release.
// Settled history the pass would skip never enters it; malformed payloads do,
// so the reader reports them instead of hiding a possible writer.
const stopCandidatePredicate = `kind = 'attempt' AND steve_attempt_stop_candidate_v1(id, state, data) != 0`
const stopCandidateQuery = `SELECT id, state, revision, data FROM operations INDEXED BY operations_attempt_stop_candidates WHERE ` + stopCandidatePredicate + ` ORDER BY updated_at DESC`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_stop_candidate_v1", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		state, ok := args[1].(string)
		if !ok || !State(state).Terminal() {
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
		r, err := decode(ledger.Operation{ID: id, State: state, Data: raw})
		if err != nil || r.Unsettled {
			return int64(1), nil
		}
		if r.State == Superseded || !strings.HasPrefix(r.Session, "ns_") || r.Node == "" || r.Execution == nil {
			return int64(0), nil
		}
		if r.SessionSettled == nil || !*r.SessionSettled || r.StopEvidence == "task-stop/"+id {
			return int64(1), nil
		}
		return int64(0), nil
	})
	ledger.MustRegisterReadIndex("operations_attempt_stop_candidates", `CREATE INDEX IF NOT EXISTS operations_attempt_stop_candidates ON operations(updated_at DESC) WHERE `+stopCandidatePredicate)
}

// StopCandidates is every live attempt and every settled one whose durable
// task stop is still owed or not yet released, most recently updated first.
// Its cost follows that set, not the settled history.
func (s *Service) StopCandidates(ctx context.Context) ([]Record, error) {
	return s.indexedRecords(ctx, stopCandidateQuery)
}

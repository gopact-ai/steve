package attempt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const (
	identityTask    = `steve_attempt_identity_v1(id,data,'task')`
	identityTurn    = `steve_attempt_identity_v1(id,data,'turn')`
	identitySession = `steve_attempt_identity_v1(id,data,'session')`
	identityStarted = `steve_attempt_identity_v1(id,data,'started')`
	identityColumns = `id,state,revision,data`
	identityOrder   = identityStarted + ` DESC,updated_at DESC,id DESC`
	taskIdentitySQL = `SELECT ` + identityColumns + ` FROM operations INDEXED BY operations_attempt_task
		WHERE kind='attempt' AND ` + identityTask + `=? ORDER BY ` + identityStarted + `,updated_at,id`
	turnAttemptsSQL = `SELECT ` + identityColumns + ` FROM operations INDEXED BY operations_attempt_turn
		WHERE kind='attempt' AND ` + identityTurn + `=? ORDER BY ` + identityOrder
	turnIdentitySQL = turnAttemptsSQL + ` LIMIT 1`
	liveTaskIdentitySQL = `SELECT ` + identityColumns + ` FROM operations INDEXED BY operations_attempt_task_live
		WHERE ` + liveAttemptPredicate + ` AND ` + identityTask + `=? ORDER BY ` + identityOrder + ` LIMIT 1`
	invalidIdentitySQL = `SELECT ` + identityColumns + ` FROM operations INDEXED BY operations_attempt_task
		WHERE kind='attempt' AND ` + identityTask + ` IS NULL LIMIT 1`
)

// Derived keys use the complete owner decoder, not a second JSON-path schema.
// Invalid rows occupy the NULL key and make every identity read fail closed.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_identity_v1", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		var raw []byte
		switch v := args[1].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		}
		r, err := decodeIdentityRecord(ledger.Operation{ID: id, Data: raw})
		if err != nil {
			return nil, nil
		}
		switch args[2] {
		case "task":
			return r.TaskID, nil
		case "turn":
			return r.TurnID, nil
		case "session":
			return sessionIdentityKey(r.Node, r.Harness, r.Session), nil
		case "started":
			return r.StartedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
		default:
			return nil, errors.New("attempt: unknown identity index field")
		}
	})
	ledger.MustRegisterReadIndex("operations_attempt_task", `CREATE INDEX IF NOT EXISTS operations_attempt_task ON operations(`+
		identityTask+`,`+identityStarted+`,updated_at,id) WHERE kind='attempt'`)
	ledger.MustRegisterReadIndex("operations_attempt_turn", `CREATE INDEX IF NOT EXISTS operations_attempt_turn ON operations(`+
		identityTurn+`,`+identityOrder+`) WHERE kind='attempt'`)
	ledger.MustRegisterReadIndex("operations_attempt_task_live", `CREATE INDEX IF NOT EXISTS operations_attempt_task_live ON operations(`+
		identityTask+`,`+identityOrder+`) WHERE `+liveAttemptPredicate)
	ledger.MustRegisterReadIndex("operations_attempt_session", `CREATE INDEX IF NOT EXISTS operations_attempt_session ON operations(`+
		identitySession+`,id) WHERE kind='attempt'`)
}

func sessionIdentityKey(node, harness, session string) string {
	raw, _ := json.Marshal([3]string{node, harness, session})
	return string(raw)
}

func decodeIdentityRecord(op ledger.Operation) (Record, error) {
	r, err := decode(op)
	if err != nil {
		return Record{}, err
	}
	if op.ID == "" || r.ID != op.ID {
		return Record{}, fmt.Errorf("attempt %s: record identity differs", op.ID)
	}
	return r, nil
}

func scanIdentityRecord(row interface{ Scan(...any) error }) (Record, error) {
	var op ledger.Operation
	var raw string
	if err := row.Scan(&op.ID, &op.State, &op.Revision, &raw); err != nil {
		return Record{}, err
	}
	op.Data = json.RawMessage(raw)
	return decodeIdentityRecord(op)
}

func checkIdentityRows(tx *ledger.ReadTx) error {
	if err := ledger.CheckOperationEnvelopesTx(tx, kind); err != nil {
		return err
	}
	r, err := scanIdentityRecord(tx.QueryRow(invalidIdentitySQL))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("attempt %s: invalid identity read index", r.ID)
}

// CheckReadable fails when any attempt cannot be decoded or is filed under
// another identity. It reads only the identity index's invalid range.
func (s *Service) CheckReadable(ctx context.Context) error {
	return s.l.Read(ctx, checkIdentityRows)
}

func (s *Service) identityRecords(ctx context.Context, query, key string) ([]Record, error) {
	var out []Record
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		if err := checkIdentityRows(tx); err != nil {
			return err
		}
		rows, err := tx.Query(query, key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanIdentityRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const sessionIdentitySQL = `SELECT id,
	(SELECT seq FROM events INDEXED BY events_operation WHERE operation_id=operations.id ORDER BY seq LIMIT 1),
	(SELECT from_state FROM events INDEXED BY events_operation WHERE operation_id=operations.id ORDER BY seq LIMIT 1)
	FROM operations INDEXED BY operations_attempt_session
	WHERE kind='attempt' AND ` + identitySession + `=?`

// LatestForSession selects the latest binding by ledger creation order, never
// wall-clock time. It reads only this session's candidate IDs and each first
// event, then decodes one payload in the same snapshot. Cost is proportional to
// the session's attempts, not all attempts or all events in their histories.
func (s *Service) LatestForSession(ctx context.Context, node, harness, session string) (Record, bool, error) {
	var result Record
	var found bool
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		if err := checkIdentityRows(tx); err != nil {
			return err
		}
		id, err := latestSessionIdentity(tx, sessionIdentityKey(node, harness, session))
		if err != nil || id == "" {
			return err
		}
		result, err = GetTx(tx, id)
		found = err == nil
		return err
	})
	return result, found, err
}

func latestSessionIdentity(tx *ledger.ReadTx, key string) (string, error) {
	rows, err := tx.Query(sessionIdentitySQL, key)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var latest string
	var lastSeq int64
	for rows.Next() {
		var id string
		var seq sql.NullInt64
		var from sql.NullString
		if err := rows.Scan(&id, &seq, &from); err != nil {
			return "", err
		}
		if !seq.Valid || seq.Int64 <= 0 || !from.Valid || from.String != "" {
			return "", fmt.Errorf("attempt %s: native session lacks its committed creation order", id)
		}
		if latest == "" || seq.Int64 > lastSeq {
			latest, lastSeq = id, seq.Int64
		} else if id != latest && seq.Int64 == lastSeq {
			return "", errors.New("native session has ambiguous execution history")
		}
	}
	return latest, rows.Err()
}

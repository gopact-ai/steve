package ledger

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"

	"modernc.org/sqlite"
)

const invalidOperationEnvelope = `steve_operation_envelope_v1(revision,incarnation,created_at,updated_at)=0`
const invalidOperationEnvelopeQuery = `SELECT id FROM operations INDEXED BY operations_invalid_envelopes
	WHERE kind=? AND ` + invalidOperationEnvelope + ` LIMIT 1`

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_operation_envelope_v1", 4, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if operationEnvelopeValid(args) {
			return int64(1), nil
		}
		return int64(0), nil
	})
	MustRegisterReadIndex("operations_invalid_envelopes", `CREATE INDEX IF NOT EXISTS operations_invalid_envelopes ON operations(kind,id) WHERE `+invalidOperationEnvelope)
}

// Match the conversions used by Operation/Operations' database/sql scans,
// including numeric affinity, then use the same ledger timestamp decoder.
// Payload JSON belongs to the domain owner, not to this envelope index.
func operationEnvelopeValid(values []driver.Value) bool {
	revision, ok := operationScalarText(values[0])
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(revision, 10, 64); err != nil {
		return false
	}
	incarnation, ok := operationScalarText(values[1])
	if !ok {
		return false
	}
	if _, err := strconv.ParseUint(incarnation, 10, 64); err != nil {
		return false
	}
	created, ok := operationScalarText(values[2])
	if !ok {
		return false
	}
	updated, ok := operationScalarText(values[3])
	if !ok {
		return false
	}
	_, _, err := stamps(created, updated)
	return err == nil
}

func operationScalarText(value driver.Value) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), true
	case bool:
		return strconv.FormatBool(value), true
	default:
		return "", false
	}
}

// CheckOperationEnvelopesTx preserves the ledger's corrupt-row refusal for
// indexed domain reads without loading otherwise irrelevant operation history.
// Call it in the same read transaction as domain selection and payload reads.
func CheckOperationEnvelopesTx(tx Reader, kind string) error {
	var id string
	err := tx.QueryRow(invalidOperationEnvelopeQuery, kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("ledger: operation %s has a corrupt envelope", id)
}

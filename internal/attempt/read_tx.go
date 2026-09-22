package attempt

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

// GetTx reads one attempt in the caller's transaction, so its identity and
// authority can be checked alongside task headers and lease tuples.
func GetTx(tx ledger.Reader, id string) (Record, error) {
	var data, state string
	var revision int64
	err := tx.QueryRow(`SELECT data,state,revision FROM operations WHERE kind='attempt' AND id=?`, id).Scan(&data, &state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, err
	}
	record, err := decode(ledger.Operation{ID: id, Data: json.RawMessage(data), State: state, Revision: revision})
	if err != nil {
		return Record{}, err
	}
	if record.ID != id {
		return Record{}, fmt.Errorf("attempt %s: record identity differs", id)
	}
	return record, nil
}

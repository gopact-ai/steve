package state

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrCloseOwed = errors.New("native session close is still owed")

// CheckTaskDeletionTx uses the deletion's snapshot rather than the store cache:
// a close recorded after the preliminary idle check still holds its authority.
func CheckTaskDeletionTx(tx ledger.Reader, taskIDs []string) error {
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='document' AND id='state'`).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var stored storedData
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("cleanup state is not one document")
	}
	for _, owed := range stored.OwedCloses {
		if owed.TaskID == "" || owed.AttemptID == "" || owed.NodeID == "" || owed.HarnessID == "" || owed.UpstreamID == "" {
			return errors.New("native close obligation has incomplete identity")
		}
		if slices.Contains(taskIDs, owed.TaskID) {
			return fmt.Errorf("%w: task %s execution %s", ErrCloseOwed, owed.TaskID, owed.AttemptID)
		}
	}
	return nil
}

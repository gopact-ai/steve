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

// OwedClosesTx reads all standing close obligations from the caller's snapshot.
// Decode the complete state document strictly before trusting any task identity;
// a never-saved document has no obligations, but an unread document is not empty.
func OwedClosesTx(tx ledger.Reader) ([]OwedClose, error) {
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind='document' AND id='state'`).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var stored *storedData
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("cleanup state is not an object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("cleanup state is not one document")
	}
	for _, owed := range stored.OwedCloses {
		if owed.TaskID == "" || owed.AttemptID == "" || owed.NodeID == "" || owed.HarnessID == "" || owed.UpstreamID == "" {
			return nil, errors.New("native close obligation has incomplete identity")
		}
	}
	return stored.OwedCloses, nil
}

// CheckTaskDeletionTx uses the deletion's snapshot rather than the store cache:
// a close recorded after the preliminary idle check still holds its authority.
func CheckTaskDeletionTx(tx ledger.Reader, taskIDs []string) error {
	owedCloses, err := OwedClosesTx(tx)
	if err != nil {
		return err
	}
	for _, owed := range owedCloses {
		if slices.Contains(taskIDs, owed.TaskID) {
			return fmt.Errorf("%w: task %s execution %s", ErrCloseOwed, owed.TaskID, owed.AttemptID)
		}
	}
	return nil
}

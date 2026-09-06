package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"github.com/gopact-ai/steve/internal/ledger"
)

// CheckHomeTx verifies a physical write target in the transaction that admits
// the write. The caller owns its operation; project owns declaration storage.
func CheckHomeTx(tx *ledger.Tx, id string, expected Home) error {
	var data []byte
	if err := tx.QueryRow("SELECT data FROM bindings WHERE kind = ? AND id = ?", kindProject, id).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %s is not active", ErrUnknown, id)
		}
		return err
	}
	var current Project
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if expected.Path == "" || current.Home.Node != expected.Node || path.Clean(current.Home.Path) != path.Clean(expected.Path) {
		return fmt.Errorf("project %s canonical directory changed before write admission", id)
	}
	return nil
}

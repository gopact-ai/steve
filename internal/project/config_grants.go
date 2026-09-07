package project

import (
	"encoding/json"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Config grants form an authoritative overlay. Removing an explicit declaration
// reveals the separately retained runtime grant, then the project's default.
func (s *Store) reconcileConfigGrants(tx *ledger.Tx, desired map[string]Project) error {
	if _, err := tx.Exec("DELETE FROM bindings WHERE kind = ?", kindConfigGrant); err != nil {
		return err
	}
	old, err := tx.Bindings(kindGrant)
	if err != nil {
		return err
	}
	for id, raw := range old {
		var g Grant
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		if g.By == "config" {
			if _, err := tx.Exec("DELETE FROM bindings WHERE kind = ? AND id = ?", kindGrant, id); err != nil {
				return err
			}
		}
	}
	for id, p := range desired {
		for principal, role := range p.ConfigGrants {
			g, err := normalizeGrant(id, principal, role, "config", s.now().UTC())
			if err != nil {
				return err
			}
			if err := tx.PutBinding(kindConfigGrant, id+"/"+g.Principal, g); err != nil {
				return err
			}
		}
	}
	return nil
}

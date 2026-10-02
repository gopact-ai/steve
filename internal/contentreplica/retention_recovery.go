package contentreplica

import (
	"database/sql"
	"errors"
)

// Workspace recoveries are operation-owned retention roots. Their owner decodes
// the complete envelope and referenced content before collection can proceed.
func (c *retentionCatalog) readRecoveryOwners(query retentionQuery) error {
	last := ""
	for {
		var id, raw string
		err := query(`SELECT id,json_object('id',id,'state',state,'revision',revision,'data',data) FROM operations WHERE kind='workspace-recovery' AND id>? ORDER BY id LIMIT 1`, last).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.addRow("workspace-recovery", id, raw); err != nil {
			return err
		}
		last = id
	}
}

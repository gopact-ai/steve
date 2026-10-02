package attempt

import (
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"path"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"modernc.org/sqlite"
)

const originalRecoveryKey = `steve_attempt_original_recovery_v1(id,data)`
const originalRecoveryIndex = "operations_attempt_original_recovery"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_original_recovery_v1", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		var raw []byte
		switch value := args[1].(type) {
		case string:
			raw = []byte(value)
		case []byte:
			raw = value
		}
		r, err := decodeIdentityRecord(ledger.Operation{ID: id, Data: raw})
		if err != nil {
			return nil, nil
		}
		if r.Abandoned == nil {
			return "", nil
		}
		// Historical AB and copy attempts without an episode remain outside
		// this association. Producers use a different Spec field entirely.
		return r.Abandoned.WorkspaceRecoveryID, nil
	})
	ledger.MustRegisterReadIndex(originalRecoveryIndex, `CREATE INDEX IF NOT EXISTS `+originalRecoveryIndex+` ON operations(`+originalRecoveryKey+`,id) WHERE kind='attempt'`)
}

func workspaceRecoverySourceID(attemptID string, target project.Home) string {
	sum := sha256.Sum256([]byte(attemptID + "\x00" + target.Node + "\x00" + path.Clean(target.Path)))
	return "workspace-recovery-" + hex.EncodeToString(sum[:16])
}

// Original sources form a set, not a time-sorted chain. The creation source
// anchors the stable episode identity; every added AB association must remain.
func validateOriginalRecoverySourcesTx(tx ledger.Reader, r WorkspaceRecovery) error {
	var invalid string
	err := tx.QueryRow(`SELECT id FROM operations INDEXED BY ` + originalRecoveryIndex + ` WHERE kind='attempt' AND ` + originalRecoveryKey + ` IS NULL LIMIT 1`).Scan(&invalid)
	if err == nil {
		return fmt.Errorf("%w: unreadable original recovery association %s", contentreplica.ErrIntegrity, invalid)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	associated, err := originalRecoverySourcesTx(tx, r.ID, len(r.Sources))
	if err != nil {
		return err
	}
	if len(associated) != len(r.Sources) {
		return fmt.Errorf("%w: original recovery source set is incomplete", contentreplica.ErrIntegrity)
	}
	created := false
	for _, source := range r.Sources {
		if !associated[source.Attempt] {
			return fmt.Errorf("%w: original source is not associated", contentreplica.ErrIntegrity)
		}
		original, err := GetTx(tx, source.Attempt)
		if err != nil {
			return fmt.Errorf("%w: missing original recovery source %s", contentreplica.ErrIntegrity, source.Attempt)
		}
		ab := original.Abandoned
		if original.TaskID != source.Task || original.Project != r.Project || original.Workspace.Project != r.Project || original.Workspace.Kind != project.KindCanonical || original.Workspace.Node != r.Target.Node || !samePhysicalPath(original.Workspace.Path, r.Target.Path) || ab == nil || ab.WorkspaceRecoveryID != r.ID || ab.ForceStopRevision != source.Revision || !ab.At.Equal(source.At) {
			return fmt.Errorf("%w: original recovery source differs from its AB decision", contentreplica.ErrIntegrity)
		}
		if workspaceRecoverySourceID(source.Attempt, r.Target) == r.ID {
			if created || !source.At.Equal(r.CreatedAt) || ab.By != r.RequestedBy {
				return fmt.Errorf("%w: recovery creation source differs", contentreplica.ErrIntegrity)
			}
			created = true
		}
	}
	if !created {
		return fmt.Errorf("%w: recovery creation source is missing", contentreplica.ErrIntegrity)
	}
	return nil
}

func originalRecoverySourcesTx(tx ledger.Reader, recoveryID string, limits ...int) (map[string]bool, error) {
	associated := map[string]bool{}
	last := ""
	for {
		var id string
		err := tx.QueryRow(`SELECT id FROM operations INDEXED BY `+originalRecoveryIndex+` WHERE kind='attempt' AND `+originalRecoveryKey+`=? AND id>? ORDER BY id LIMIT 1`, recoveryID, last).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return associated, nil
		}
		if err != nil {
			return nil, err
		}
		associated[id], last = true, id
		if len(limits) > 0 && len(associated) > limits[0] {
			return associated, nil
		}
	}
}

package turn

import (
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// The exact slot is read under the transaction that freezes accounting. An
// absent slot must never be reinterpreted as one installed after abandonment.
func abandonContextTx(tx *ledger.Tx, id string) (attempt.AbandonContext, error) {
	r, err := attempt.GetTx(tx, id)
	if err != nil {
		return attempt.AbandonContext{}, err
	}
	tracked, found, err := task.GetTx(tx, r.TaskID)
	if err != nil {
		return attempt.AbandonContext{}, err
	}
	if !found {
		return attempt.AbandonContext{}, errors.New("original session task is missing")
	}
	source, err := state.AbandonSourceTx(tx, tracked.Channel, r.Agent)
	if err != nil {
		return attempt.AbandonContext{}, err
	}
	out := attempt.AbandonContext{State: source.State}
	if source.State == "absent" {
		return out, nil
	}
	session := source.Session
	original := session.NodeID == r.Node && session.HarnessID == r.Harness && session.Workspace == r.Workspace.Path && session.ProjectID == r.Project
	if r.Session != "" {
		original = original && session.UpstreamID == r.Session
	} else {
		original = original && session.UpstreamID == "" && r.NativeImport != nil && source.ImportFingerprint == state.NativeImportFingerprint(r.NativeImport)
	}
	out.State = "different"
	if original {
		out.State, out.Fingerprint, out.ImportFingerprint = "original", source.Fingerprint, source.ImportFingerprint
	}
	return out, nil
}

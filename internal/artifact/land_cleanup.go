package artifact

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/gopact-ai/steve/internal/ledger"
)

// closeUnappliedLanding runs only as the sole nonrecoverable landing caller
// returns. The persisted state, rather than the in-memory merge, proves no
// apply was admitted. Closing that record needs no canonical write lease;
// in particular a parent may already have released a borrowed lease.
// Applying WALs, real terminal conflicts and recoverable drivers stay intact.
func (s *Store) closeUnappliedLanding(ctx context.Context, land *Landing, cause error) {
	ctx = context.WithoutCancel(ctx)
	op, exists, err := s.ledger.Operation(ctx, land.ID)
	if err == nil && exists && op.Kind == landKind {
		var stored Landing
		err = json.Unmarshal(op.Data, &stored)
		if err == nil && !stored.Recoverable && (op.State == LandProposed || op.State == LandLocked || op.State == LandMerged) {
			stored.State, stored.Lease, stored.Error = LandMergeConflicted, nil, "interrupted before apply: "+cause.Error()
			stored.Unapplied = true
			stored.EndedAt = s.now().UTC()
			_, err = s.ledger.Transition(ctx, op.ID, op.State, stored.State, land.By, nil, nil,
				func(tx *ledger.Tx, op *ledger.Operation) error { return tx.SetData(op, stored) })
			if err == nil {
				*land = stored
			}
		}
	}
	if err != nil {
		slog.Error("artifact: interrupted landing cleanup failed", "landing", land.ID, "error", err)
	}
}

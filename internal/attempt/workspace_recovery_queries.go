package attempt

import (
	"context"

	"github.com/gopact-ai/steve/internal/ledger"
)

func (s *Service) WorkspaceRecoveries(ctx context.Context) ([]WorkspaceRecovery, error) {
	var out []WorkspaceRecovery
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error { var err error; out, err = workspaceRecoveriesTx(tx); return err })
	return out, err
}

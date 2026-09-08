package app

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
)

// openLedger opens the authority every store lives in. A database older
// than its incarnation file — a restore from backup — is refused here with
// the command that fixes it, rather than served with stale leases.
func openLedger(cfg *config.Config) (*ledger.Ledger, error) {
	book, err := ledger.Open(filepath.Dir(cfg.Gateway.StatePath), ledger.Options{})
	if errors.Is(err, ledger.ErrRecoveryRequired) {
		return nil, fmt.Errorf("%w\nrun: steve ledger recover --config <config>", err)
	}
	return book, err
}

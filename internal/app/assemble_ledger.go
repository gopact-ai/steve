package app

import (
	"fmt"
	"log"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
)

func assembleLedger(boot runtimeAssembly) (ledgerAssembly, error) {
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	if cfg.Gateway.Region != "" {
		book.SetRegion(cfg.Gateway.Region)
	}
	for name, region := range cfg.Gateway.Regions {
		book.RegisterIssuer(name, ledger.NewHTTPIssuer(region.URL, region.Token))
	}
	attempts := attempt.New(book)
	recovery, err := attempts.PrepareRecovery(ctx, "hub startup")
	if err != nil {
		return nil, fmt.Errorf("prepare previous executions for recovery: %w", err)
	}
	quarantinedTasks := make(map[string]bool, len(recovery.Quarantined))
	for _, r := range recovery.Quarantined {
		quarantinedTasks[r.TaskID] = true
		log.Printf("steve: quarantined previous writer: %s", attempt.Describe(r))
	}
	for _, r := range recovery.Expired {
		log.Printf("steve: recovered settled attempt: %s", attempt.Describe(r))
	}
	store, err := state.OpenLedger(book, cfg.Gateway.StatePath)
	if err != nil {
		return nil, err
	}
	return &ledgerValues{attempts: attempts, quarantinedTasks: quarantinedTasks, store: store}, nil
}

type ledgerAssembly interface {
	Attempts() *attempt.Service
	QuarantinedTasks() map[string]bool
	Store() *state.Store
}

type ledgerValues struct {
	attempts         *attempt.Service
	quarantinedTasks map[string]bool
	store            *state.Store
}

func (v *ledgerValues) Attempts() *attempt.Service { return v.attempts }

func (v *ledgerValues) QuarantinedTasks() map[string]bool { return v.quarantinedTasks }

func (v *ledgerValues) Store() *state.Store { return v.store }

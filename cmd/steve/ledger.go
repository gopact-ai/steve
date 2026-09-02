package main

import (
	"context"
	"errors"
	"flag"
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

// ledgerCmd is the operator's door into the ledger: inspect the incarnation,
// rotate it after restoring a backup, and reconcile the effects journal
// before the gateway serves again.
func ledgerCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: steve ledger status|rotate|recover [--config config.json]")
	}
	verb := args[0]
	flags := flag.NewFlagSet("ledger "+verb, flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfg.Gateway.StatePath)
	switch verb {
	case "status":
		book, err := ledger.Open(dir, ledger.Options{})
		if errors.Is(err, ledger.ErrRecoveryRequired) {
			fmt.Printf("ledger: %s\nrecovery required: run steve ledger recover\n", dir)
			return nil
		}
		if err != nil {
			return err
		}
		defer book.Close()
		outcomes, err := book.Journal().Reconcile()
		if err != nil {
			return err
		}
		unknown := 0
		for _, o := range outcomes {
			if !o.Known() {
				unknown++
			}
		}
		fmt.Printf("ledger: %s\nincarnation: %d\neffects: %d recorded, %d outcome-unknown\n", dir, book.Incarnation(), len(outcomes), unknown)
		return nil
	case "rotate":
		next, err := ledger.Rotate(dir)
		if err != nil {
			return err
		}
		fmt.Printf("incarnation rotated to %d; run steve ledger recover before serving\n", next)
		return nil
	case "recover":
		book, err := ledger.Open(dir, ledger.Options{Recover: true})
		if err != nil {
			return err
		}
		defer book.Close()
		ctx := context.Background()
		if err := book.InvalidateAll(ctx); err != nil {
			return err
		}
		outcomes, err := book.Journal().Reconcile()
		if err != nil {
			return err
		}
		for _, o := range outcomes {
			if o.Known() {
				continue
			}
			if o.Started == nil || o.Started.Incarnation == book.Incarnation() {
				continue // this incarnation's own in-flight effects are not a restore's problem
			}
			fmt.Printf("outcome-unknown: %s (started %s)\n", o.Effect, o.Started.At.Format("2006-01-02 15:04:05"))
		}
		book.RecoveryDone()
		fmt.Printf("ledger at incarnation %d: every lease invalidated, %d effects reviewed\n", book.Incarnation(), len(outcomes))
		return nil
	}
	return fmt.Errorf("unknown ledger verb %q", verb)
}

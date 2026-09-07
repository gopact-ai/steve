package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
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
		return errors.New("usage: steve ledger status|rotate|recover|effects|resolve|quarantine|confirm-stopped|clones|confirm-clone-stopped [--config config.json]")
	}
	verb := args[0]
	flags := flag.NewFlagSet("ledger "+verb, flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	evidence := flags.String("evidence", "", "verified physical termination evidence for confirm-stopped")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfg.Gateway.StatePath)
	switch verb {
	case "clones", "confirm-clone-stopped":
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			return err
		}
		defer book.Close()
		projects := project.Open(book)
		if verb == "clones" {
			ops, err := projects.CloneOperations(context.Background())
			if err != nil {
				return err
			}
			for _, op := range ops {
				fmt.Printf("%s  state=%s  project=%s  node=%s  path=%s  %s\n", op.ID, op.State, op.Project, op.Copy.Node, op.Copy.Path, op.Error)
			}
			return nil
		}
		if len(flags.Args()) != 1 || *evidence == "" {
			return errors.New("usage: steve ledger confirm-clone-stopped --config <config> --evidence <verified clone process exit> <operation id>")
		}
		if err := projects.ConfirmCloneStopped(context.Background(), flags.Args()[0], "operator", *evidence); err != nil {
			return err
		}
		fmt.Printf("%s: clone stop evidence recorded; directory quarantine cleared\n", flags.Args()[0])
		return nil
	case "quarantine", "confirm-stopped":
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			return err
		}
		defer book.Close()
		service := attempt.New(book)
		if verb == "quarantine" {
			records, err := service.Unsettled(context.Background())
			if err != nil {
				return err
			}
			for _, r := range records {
				fmt.Printf("%s  task=%s  node=%s  harness=%s  workspace=%s  %s\n", r.ID, r.TaskID, r.Node, r.Harness, r.Workspace.Path, r.Error)
			}
			if len(records) == 0 {
				fmt.Println("no quarantined writers")
			}
			return nil
		}
		if len(flags.Args()) != 1 || *evidence == "" {
			return errors.New("usage: steve ledger confirm-stopped --config <config> --evidence <verified process exit> <attempt id>; verify the original writer stopped, then restart the hub after reconciliation")
		}
		r, err := service.ConfirmStopped(context.Background(), flags.Args()[0], "operator", *evidence)
		if err != nil {
			return err
		}
		fmt.Printf("%s: physical stop evidence recorded; quarantine cleared; restart the hub to reload runtime state\n", r.ID)
		return nil
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
	case "effects":
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			return err
		}
		defer book.Close()
		unresolved, err := intent.New(book).Unresolved(context.Background())
		if err != nil {
			return err
		}
		outcomes, err := book.Journal().Reconcile()
		if err != nil {
			return err
		}
		for _, it := range unresolved {
			fmt.Printf("intent %s  %s  task #%s attempt %s  %s  %s\n", it.ID, it.Tool, it.TaskID, it.AttemptID, it.At.Format(time.RFC3339), it.Error)
		}
		for _, o := range outcomes {
			if !o.Known() && o.Effect.Kind != "dispatch" && o.Started != nil {
				fmt.Printf("effect %s  started %s  outcome unknown\n", o.Effect, o.Started.At.Format(time.RFC3339))
			}
		}
		if len(unresolved) == 0 {
			fmt.Println("no intents with an unknown outcome")
		}
		return nil
	case "resolve":
		rest := flags.Args()
		if len(rest) != 2 {
			return errors.New("usage: steve ledger resolve <intent id> happened|new")
		}
		book, err := ledger.Open(dir, ledger.Options{})
		if err != nil {
			return err
		}
		defer book.Close()
		it, err := intent.New(book).Resolve(context.Background(), rest[0], rest[1], "operator")
		if err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", it.ID, it.State)
		return nil
	}
	return fmt.Errorf("unknown ledger verb %q", verb)
}

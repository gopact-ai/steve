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
	// rotate must work on a ledger that refuses to open and status says why one did; the rest run on one opened here.
	switch verb {
	case "status":
		return ledgerStatus(dir)
	case "rotate":
		return ledgerRotate(dir)
	case "resolve":
		if len(flags.Args()) != 2 {
			return errors.New("usage: steve ledger resolve <intent id> happened|new")
		}
	}
	run, ok := ledgerVerbs[verb]
	if !ok {
		return fmt.Errorf("unknown ledger verb %q", verb)
	}
	// Only recover may open a ledger that predates its incarnation file.
	book, err := ledger.Open(dir, ledger.Options{Recover: verb == "recover"})
	if err != nil {
		return err
	}
	defer book.Close()
	return run(context.Background(), book, flags.Args(), *evidence)
}

// ledgerVerbs run on an open ledger with the operator's positional arguments and --evidence.
var ledgerVerbs = map[string]func(ctx context.Context, book *ledger.Ledger, rest []string, evidence string) error{
	"recover": ledgerRecover, "effects": ledgerEffects, "resolve": ledgerResolve, "quarantine": ledgerQuarantine,
	"confirm-stopped": ledgerConfirmStopped, "clones": ledgerClones, "confirm-clone-stopped": ledgerConfirmCloneStopped,
}

func ledgerStatus(dir string) error {
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
}

func ledgerRotate(dir string) error {
	next, err := ledger.Rotate(dir)
	if err != nil {
		return err
	}
	fmt.Printf("incarnation rotated to %d; run steve ledger recover before serving\n", next)
	return nil
}

// ledgerRecover invalidates every lease and lists the effects an earlier incarnation left unknown.
func ledgerRecover(ctx context.Context, book *ledger.Ledger, _ []string, _ string) error {
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

func ledgerEffects(ctx context.Context, book *ledger.Ledger, _ []string, _ string) error {
	unresolved, err := intent.New(book).Unresolved(ctx)
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
}

func ledgerResolve(ctx context.Context, book *ledger.Ledger, rest []string, _ string) error {
	it, err := intent.New(book).Resolve(ctx, rest[0], rest[1], "operator")
	if err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", it.ID, it.State)
	return nil
}

func ledgerQuarantine(ctx context.Context, book *ledger.Ledger, _ []string, _ string) error {
	records, err := attempt.New(book).Unsettled(ctx)
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

func ledgerConfirmStopped(ctx context.Context, book *ledger.Ledger, rest []string, evidence string) error {
	if len(rest) != 1 || evidence == "" {
		return errors.New("usage: steve ledger confirm-stopped --config <config> --evidence <verified process exit> <attempt id>; verify the original writer stopped, then restart the hub after reconciliation")
	}
	r, err := attempt.New(book).ConfirmStopped(ctx, rest[0], "operator", evidence)
	if err != nil {
		return err
	}
	fmt.Printf("%s: physical stop evidence recorded; quarantine cleared; restart the hub to reload runtime state\n", r.ID)
	return nil
}

func ledgerClones(ctx context.Context, book *ledger.Ledger, _ []string, _ string) error {
	ops, err := project.Open(book).CloneOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		fmt.Printf("%s  state=%s  project=%s  node=%s  path=%s  %s\n", op.ID, op.State, op.Project, op.Copy.Node, op.Copy.Path, op.Error)
	}
	return nil
}

func ledgerConfirmCloneStopped(ctx context.Context, book *ledger.Ledger, rest []string, evidence string) error {
	if len(rest) != 1 || evidence == "" {
		return errors.New("usage: steve ledger confirm-clone-stopped --config <config> --evidence <verified clone process exit> <operation id>")
	}
	if err := project.Open(book).ConfirmCloneStopped(ctx, rest[0], "operator", evidence); err != nil {
		return err
	}
	fmt.Printf("%s: clone stop evidence recorded; directory quarantine cleared\n", rest[0])
	return nil
}

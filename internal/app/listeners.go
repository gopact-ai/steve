package app

import (
	"context"
	"fmt"
	"log"

	"github.com/gopact-ai/steve/internal/debugapi"
	"github.com/gopact-ai/steve/internal/desktop"
)

func startListeners(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, page consoleAssembly, management administrationAssembly, delegates delegationAssembly, channels channelsAssembly) error {
	configPath := input.ConfigPath()
	environment := input.Environment()
	background := boot.Background()
	cfg := boot.Config()
	ctx := boot.Context()
	manager := boot.Manager()
	attempts := storage.Attempts()
	coordinator := work.Coordinator()
	gw := work.Gateway()
	schedules := work.Schedules()
	tasks := work.Tasks()
	admin := page.Admin()
	cons := page.Console()
	dashboard := page.Dashboard()
	reconciliations := page.Reconciliations()
	services := management.Services()
	recoverRetainedDelegates := delegates.RecoverRetainedDelegates()
	redeliverPending := delegates.RedeliverPending()
	channel := channels.Channel()
	// Children that ended before the last process died, whose parents
	// were never told.
	if redeliverPending != nil {
		background.Go(redeliverPending)
	}
	if recoverRetainedDelegates != nil {
		reconciliations.Go(func() { runReconciler(ctx, "reconcile retained child executions", recoverRetainedDelegates) })
	}
	if environment != nil {
		stops := newApplicationStops(attempts, tasks, manager)
		reconciliations.Go(func() { runReconciler(ctx, "reconcile requested task stops", stops.Reconcile) })
	}
	if err := services.Ready(); err != nil {
		return fmt.Errorf("record service readiness: %w", err)
	}
	if environment == nil {
		removeEndpoint, err := desktop.PublishEndpoint(*configPath, dashboard.URL())
		if err != nil {
			return fmt.Errorf("publish desktop endpoint: %w", err)
		}
		life.Defer(func() { removeEndpoint() })
	} else if environment.Ready != nil {
		if err := environment.Ready(admin, dashboard); err != nil {
			return err
		}
	}
	background.Go(func(context.Context) {
		if err := dashboard.Serve(); err != nil {
			log.Printf("steve: read model: %v", err)
		}
	})
	log.Printf("steve: dashboard on %s  (steve top -url %s)", dashboard.URL(), dashboard.URL())

	background.Go(func(ctx context.Context) { runScheduleDispatcher(ctx, schedules, cons, gw, coordinator) })

	if addr := cfg.Gateway.DebugAddr; addr != "" && channel != nil {
		go func() {
			if err := debugapi.Serve(ctx, addr, gw, debugapi.Defaults{
				ChatID:       cfg.Gateway.DebugChatID,
				SenderOpenID: cfg.Feishu.OwnerOpenID,
				Sender:       channel,
			}); err != nil {
				log.Printf("steve: %v", err)
			}
		}()
	}
	return nil
}

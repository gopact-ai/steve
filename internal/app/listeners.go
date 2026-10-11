package app

import (
	"context"
	"fmt"
	"log/slog"

	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/debugapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/turn"
)

func startListeners(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, page consoleAssembly, management administrationAssembly, delegates delegationAssembly, channels channelsAssembly) error {
	configPath := input.ConfigPath()
	environment := input.Environment()
	background := boot.Background()
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
	reconcileDeliveries := delegates.ReconcileDeliveries()
	channel := channels.Channel()
	receivers := delegates.TaskChannels()
	// The console starts serving below and may rewrite the configuration
	// from then on; take what the listeners need from it first.
	cfg := boot.Config()
	debugAddr := cfg.Gateway.DebugAddr
	debugDefaults := debugapi.Defaults{ChatID: cfg.Gateway.DebugChatID, SenderOpenID: cfg.Feishu.OwnerOpenID, Sender: channel}
	// Children that ended before the last process died, whose parents
	// were never told.
	if reconcileDeliveries != nil {
		reconciliations.Go(func() { runReconciler(ctx, "reconcile child result delivery", reconcileDeliveries) })
	}
	if recoverRetainedDelegates != nil {
		reconciliations.Go(func() { runReconciler(ctx, "reconcile retained child executions", recoverRetainedDelegates) })
	}
	if environment != nil {
		reconciliations.Go(func() { runReconciler(ctx, "project abandoned executions", cons.ReconcileAbandonments) })
		stops := newApplicationStops(attempts, tasks, manager, work.CatalogText())
		stops.executions = work.Executions()
		stops.restarts = environment.MemberRestarts
		reconciliations.Go(func() { runReconciler(ctx, "reconcile requested task stops", stops.Reconcile) })
		// Sessions a conversation let go of while their node could not be
		// reached are closed once it can be.
		owed := newApplicationOwedCloses(storage.Store(), attempts, tasks, manager)
		reconciliations.Go(func() { runReconciler(ctx, "reconcile owed session closes", owed.Reconcile) })
		control := turn.NewWorkspaceRecoveryControl(coordinator)
		recovery := &applicationWorkspaceRecovery{attempts: attempts, artifacts: work.Artifacts(), state: storage.Store(), sessions: manager, control: control}
		reconciliations.Go(func() { runReconciler(ctx, "reconcile workspace recovery", recovery.Reconcile) })
	}
	for name, adapter := range work.NativeAdapters() {
		instance := work.NativeChannels()[name]
		if instance == nil {
			return fmt.Errorf("installed channel %s has no gateway", name)
		}
		background.Go(func(ctx context.Context) {
			err := adapter.Run(ctx, messagechannel.Handlers{Input: instance.HandleMessage, Action: instance.HandleCardAction}, func() {
				instance.BindAdapter(adapter)
				if gate := delegates.Gate(); gate != nil {
					gate.BindChannel(name, adapter)
				}
			})
			if err != nil && ctx.Err() == nil {
				view := page.Admin().View
				view.Observe("channel.error", name, "Installed channel stopped; its accepted input remains pending", map[string]string{"channel": name})
			}
		})
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
			slog.Error(fmt.Sprintf("steve: read model: %v", err))
		}
	})
	// The console needs its token, which stays out of logs; dash prints the
	// signed-in address from the same configuration.
	slog.Info(fmt.Sprintf("steve: console on %s  (open it: steve dash -config %q)", dashboard.URL(), *configPath))

	background.Go(func(ctx context.Context) { runScheduleDispatcher(ctx, schedules, receivers, coordinator) })

	if debugAddr != "" && channel != nil {
		background.Go(func(ctx context.Context) {
			if err := debugapi.Serve(ctx, debugAddr, gw, debugDefaults); err != nil {
				slog.Error(fmt.Sprintf("steve: %v", err))
			}
		})
	}
	return nil
}

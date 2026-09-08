package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func assembleFleetWorkers(boot runtimeAssembly, storage ledgerAssembly, machines fleetAssembly, modelInfo modelsAssembly, work executionAssembly, projection readModelAssembly) error {
	background := boot.Background()
	ctx := boot.Context()
	attempts := storage.Attempts()
	nodes := machines.Nodes()
	projects := machines.Projects()
	endpoints := modelInfo.Endpoints()
	prober := modelInfo.Prober()
	artifacts := work.Artifacts()
	tasks := work.Tasks()
	repos := projection.Repos()
	view := projection.View()

	// Dial the fleet now and keep redialing what is down. Without this the
	// registry only connects when something asks it to, so a hub that has
	// just started would report every node as down and refuse every
	// placement — describing its own ignorance rather than the fleet.
	nodes.Start(ctx)
	// What earlier processes and dropped connections left behind: the
	// hub's own orphaned worktrees now, queued landings from here on.
	background.Go(func(ctx context.Context) { sweepWorktrees(ctx, artifacts, attempts, tasks, view, "", "") })
	background.Go(func(ctx context.Context) { sweepLandings(ctx, projects, artifacts, view) })
	background.Go(repos.Run)
	background.Go(func(ctx context.Context) { sweepIdleTasks(ctx, tasks, attempts, view) })
	// Discover models for whatever nobody has run yet. It is discovery,
	// not work: a session opened and closed, no prompt sent. Done off the
	// startup path so a slow adapter never delays the first message.
	background.Go(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		for _, r := range prober.ProbeAll(ctx, endpoints(ctx), false) {
			if r.Err != nil {
				slog.Warn(fmt.Sprintf("steve: probe %s/%s: %v", nodewire.Place(r.Endpoint.Node), r.Endpoint.Harness, r.Err), "node", nodewire.Place(r.Endpoint.Node), "harness", r.Endpoint.Harness)
				continue
			}
			slog.Info(fmt.Sprintf("steve: %s/%s runs %q, offers %v", nodewire.Place(r.Endpoint.Node), r.Endpoint.Harness, r.Current, r.Available), "node", nodewire.Place(r.Endpoint.Node), "harness", r.Endpoint.Harness)
		}
	})
	return nil
}

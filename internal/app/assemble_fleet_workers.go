package app

import (
	"context"
	"github.com/gopact-ai/steve/internal/turn"
)

func assembleFleetWorkers(boot runtimeAssembly, storage ledgerAssembly, machines fleetAssembly, modelInfo modelsAssembly, work executionAssembly, projection readModelAssembly) error {
	background := boot.Background()
	ctx := boot.Context()
	attempts := storage.Attempts()
	nodes := machines.Nodes()
	projects := machines.Projects()
	artifacts := work.Artifacts()
	coordinator := work.Coordinator()
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
	background.Go(func(ctx context.Context) {
		sweepWorktrees(ctx, artifacts, attempts, tasks, view, boot.NodeName(), "", "")
	})
	// A landing that stops at a merge conflict used to sit in the queue
	// being recomputed every pass. Now the same sweep that retries it can
	// hand it to an agent, in the half-merged tree git kept.
	background.Go(func(ctx context.Context) { sweepLandings(ctx, projects, artifacts, view, coordinator) })
	background.Go(repos.Run)
	idleClose := turn.IdleCloseReservation(coordinator)
	background.Go(func(ctx context.Context) {
		sweepIdleTasks(ctx, boot.Book(), tasks, attempts, view, idleClose)
	})
	// Registered launch configurations are not permission to execute them.
	// Sessions report their actual settings; explicit model probes remain
	// available without starting every configured agent during boot.
	return nil
}

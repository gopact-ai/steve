package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

// The two sweepers clean up what a dropped connection or a dead parent
// turn leaves behind: worktrees nobody owns, and results queued to land
// that no turn comes back for.

// landEvery is how often queued landings are retried when no turn on
// the project has landed them.
const landEvery = 30 * time.Second

// sweepLandings lands what delegations left queued for every project,
// whenever the project's canonical lock is free. A turn in progress
// holds that lock and lands the queue itself when it ends; this covers
// the parent that died and the project nobody spoke to again.
func sweepLandings(ctx context.Context, projects *project.Store, artifacts *artifact.Store, view *readmodel.Model) {
	ticker := time.NewTicker(landEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		list, err := projects.List(ctx)
		if err != nil {
			continue
		}
		for _, p := range list {
			landed, err := artifacts.LandPending(ctx, p)
			for _, l := range landed {
				view.Observe("landing", p.ID, fmt.Sprintf("queued result %s landed into %s: %s (%d paths)", l.Artifact[:12], p.ID, l.State, len(l.Paths)))
				log.Printf("sweep: landing %s of %s into %s: %s (%d paths)", l.ID, l.Artifact[:12], p.ID, l.State, len(l.Paths))
			}
			if err != nil && !errors.Is(err, ledger.ErrHeld) {
				log.Printf("sweep: land pending for %s: %v", p.ID, err)
			}
		}
	}
}

// liveWorktrees are the worktree paths some live attempt still owns.
func liveWorktrees(ctx context.Context, attempts *attempt.Service) map[string]bool {
	keep := map[string]bool{}
	live, err := attempts.Live(ctx)
	if err != nil {
		return nil // unknown: sweep nothing rather than something owned
	}
	for _, r := range live {
		if r.Workspace.Path != "" {
			keep[r.Workspace.Path] = true
		}
	}
	return keep
}

// sweepWorktrees removes the orphaned worktrees on one machine ("" is
// the hub) and says what it took.
func sweepWorktrees(ctx context.Context, artifacts *artifact.Store, attempts *attempt.Service, tasks *task.Store, view *readmodel.Model, node, root string) {
	keep := liveWorktrees(ctx, attempts)
	if keep == nil {
		return
	}
	physical := node
	if physical == "" {
		physical = nodeName()
	}
	prepared, err := attempts.RelocationWorkspaces(ctx, physical)
	if err != nil {
		log.Printf("sweep: pending recovery workspaces on %s: %v", physical, err)
		return
	}
	removed, err := artifacts.SweepWorktrees(ctx, node, root, func(path string) bool {
		return keep[path] || prepared[path] || tasks != nil && tasks.KeepsRecoveryWorkspace(physical, path)
	})
	where := node
	if where == "" {
		where = nodeName()
	}
	if err != nil {
		log.Printf("sweep: worktrees on %s: %v", where, err)
	}
	if len(removed) > 0 {
		view.Observe("worktree.sweep", where, fmt.Sprintf("removed %d orphaned worktree(s): %s", len(removed), strings.Join(removed, ", ")))
		log.Printf("sweep: removed %d orphaned worktree(s) on %s: %s", len(removed), where, strings.Join(removed, ", "))
	}
}

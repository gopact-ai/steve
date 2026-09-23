package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
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
func sweepLandings(ctx context.Context, projects *project.Store, artifacts *artifact.Store, view *readmodel.Model, mender conflictMender) {
	ticker := time.NewTicker(landEvery)
	defer ticker.Stop()
	var mending atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// A landing interrupted mid-apply is finished first: until it is,
		// its project's canonical is half written and takes no new landing.
		recovered, err := artifacts.RetryRecoveries(ctx)
		for _, l := range recovered {
			slog.Info(fmt.Sprintf("sweep: recovered landing %s of %s into %s: %s (%d paths)", l.ID, l.Artifact, l.Project, l.State, len(l.Paths)), "landing", l.ID, "artifact", l.Artifact, "project", l.Project, "state", l.State)
		}
		if err != nil {
			slog.Error(fmt.Sprintf("sweep: retry landing recoveries: %v", err), "error", err.Error())
		}
		list, err := projects.List(ctx)
		if err != nil {
			continue
		}
		for _, p := range list {
			landed, err := artifacts.LandPending(ctx, p)
			for _, l := range landed {
				// Keys: artifact, project, state, paths.
				view.Observe("landing", p.ID, fmt.Sprintf("queued result %s landed into %s: %s (%d paths)", l.Artifact[:12], p.ID, l.State, len(l.Paths)),
					map[string]string{"artifact": l.Artifact[:12], "project": p.ID, "state": l.State, "paths": strconv.Itoa(len(l.Paths))})
				slog.Info(fmt.Sprintf("sweep: landing %s of %s into %s: %s (%d paths)", l.ID, l.Artifact[:12], p.ID, l.State, len(l.Paths)), "landing", l.ID, "artifact", l.Artifact, "project", p.ID)
			}
			// A busy lock and a recovery still to finish are retried next
			// pass; the recovery logs its own reason.
			if err != nil && !errors.Is(err, ledger.ErrHeld) && !errors.Is(err, artifact.ErrRecoveryPending) {
				slog.Error(fmt.Sprintf("sweep: land pending for %s: %v", p.ID, err), "project", p.ID)
			}
		}
		// Resolving a conflict is a plan: minutes of agent work. It runs
		// beside the sweep rather than inside it, so a queue on one project
		// is not held up by a conflict on another, and only one pass is in
		// flight however long it takes.
		if mender != nil && mending.CompareAndSwap(false, true) {
			go func() {
				defer mending.Store(false)
				for _, p := range list {
					resolveConflicts(ctx, mender, view, p)
				}
			}()
		}
	}
}

// conflictMender hands a landing stuck on a merge conflict to an agent.
// The turn coordinator is the one that can: it owns the plan supervisor.
type conflictMender interface {
	AutoResolveConflicts(ctx context.Context, p project.Project) []turn.Resolution
}

// resolveConflicts asks for the project's stuck landings to be resolved.
// It costs nothing when nothing is stuck, and the coordinator refuses a
// conflict it is already working on, so a resolution that outlives the
// sweep interval is not started twice.
func resolveConflicts(ctx context.Context, mender conflictMender, view *readmodel.Model, p project.Project) {
	if mender == nil {
		return
	}
	for _, r := range mender.AutoResolveConflicts(ctx, p) {
		if r.Err != nil {
			// Keys: artifact, project, landing.
			view.Observe("landing", p.ID, fmt.Sprintf("merge conflict on %s in %s was not resolved: %v", r.Artifact[:12], p.ID, r.Err),
				map[string]string{"artifact": r.Artifact[:12], "project": p.ID, "landing": r.Landing})
			slog.Warn(fmt.Sprintf("sweep: resolve conflict %s in %s: %v", r.Artifact[:12], p.ID, r.Err), "artifact", r.Artifact, "project", p.ID, "landing", r.Landing)
			continue
		}
		view.Observe("landing", p.ID, fmt.Sprintf("merge conflict on %s in %s resolved by %s", r.Artifact[:12], p.ID, r.Agent),
			map[string]string{"artifact": r.Artifact[:12], "project": p.ID, "landing": r.Landing})
		slog.Info(fmt.Sprintf("sweep: %s resolved the merge conflict on %s in %s", r.Agent, r.Artifact[:12], p.ID), "artifact", r.Artifact, "project", p.ID, "agent", r.Agent)
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
		physical = adminsvc.NodeName()
	}
	prepared, err := attempts.RelocationWorkspaces(ctx, physical)
	if err != nil {
		slog.Error(fmt.Sprintf("sweep: pending recovery workspaces on %s: %v", physical, err), "node", physical)
		return
	}
	removed, err := artifacts.SweepWorktrees(ctx, node, root, func(path string) bool {
		return keep[path] || prepared[path] || tasks != nil && tasks.KeepsRecoveryWorkspace(physical, path)
	})
	where := node
	if where == "" {
		where = adminsvc.NodeName()
	}
	if err != nil {
		slog.Error(fmt.Sprintf("sweep: worktrees on %s: %v", where, err), "node", where)
	}
	if len(removed) > 0 {
		// Keys: count, items.
		view.Observe("worktree.sweep", where, fmt.Sprintf("removed %d orphaned worktree(s): %s", len(removed), strings.Join(removed, ", ")),
			map[string]string{"count": strconv.Itoa(len(removed)), "items": strings.Join(removed, "\n")})
		slog.Info(fmt.Sprintf("sweep: removed %d orphaned worktree(s) on %s: %s", len(removed), where, strings.Join(removed, ", ")), "node", where)
	}
}

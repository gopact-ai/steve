// Resolving landings that stopped at a merge conflict, from the console.

package admin

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// ResolveConflicts hands every landing of the project that is stuck on a
// merge conflict to an agent. Resolving one is a plan step that runs for
// minutes, so the request only starts the work and says how much it
// started; the plan reports itself the way every other plan does.
func (a *Service) ResolveConflicts(ctx context.Context, projectID string) (consoleapi.ResolveConflictsResult, error) {
	var out consoleapi.ResolveConflictsResult
	if a.Artifacts == nil || a.Coordinator == nil || a.Projects == nil {
		return out, fmt.Errorf("conflict resolution is not wired")
	}
	p, ok, err := a.Projects.Get(ctx, projectID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, fmt.Errorf("no project %s", projectID)
	}
	stuck, err := a.Artifacts.Stuck(ctx, projectID)
	if err != nil {
		return out, err
	}
	for _, s := range stuck {
		if !s.Resolvable() {
			out.Skipped = append(out.Skipped, s.Artifact)
			continue
		}
		out.Started++
	}
	if out.Started == 0 {
		return out, nil
	}
	// Detached from the request: the browser is not going to hold a
	// connection open for a plan, and cancelling the fetch must not
	// cancel a resolution that is already editing files.
	background := a.Lifetime
	if background == nil {
		background = context.WithoutCancel(ctx)
	}
	go func() {
		for _, r := range a.Coordinator.ResolveConflicts(background, p) {
			if r.Err != nil {
				slog.Warn(fmt.Sprintf("admin: resolve conflict %s in %s: %v", r.Artifact, projectID, r.Err), "artifact", r.Artifact, "project", projectID)
			}
		}
	}()
	return out, nil
}

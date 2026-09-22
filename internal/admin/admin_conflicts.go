// Resolving landings that stopped at a merge conflict, from the console.

package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/project"
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
	background := a.background(ctx)
	go func() {
		for _, r := range a.Coordinator.ResolveConflicts(background, p) {
			if r.Err != nil {
				slog.Warn(fmt.Sprintf("admin: resolve conflict %s in %s: %v", r.Artifact, projectID, r.Err), "artifact", r.Artifact, "project", projectID)
			}
		}
	}()
	return out, nil
}

// ResolveAllConflicts hands every stuck result, in every project, to an
// agent. The console lists conflicts across the whole workspace, so the
// action offered beside that list has to cover the whole workspace too.
func (a *Service) ResolveAllConflicts(ctx context.Context) (consoleapi.ResolveConflictsResult, error) {
	var out consoleapi.ResolveConflictsResult
	if a.Artifacts == nil || a.Coordinator == nil || a.Projects == nil {
		return out, fmt.Errorf("conflict resolution is not wired")
	}
	stuck, err := a.Artifacts.AllStuck(ctx)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	var projects []project.Project
	for _, s := range stuck {
		if !s.Resolvable() {
			out.Skipped = append(out.Skipped, s.Artifact)
			continue
		}
		out.Started++
		if seen[s.Project] {
			continue
		}
		p, ok, err := a.Projects.Get(ctx, s.Project)
		if err != nil || !ok {
			continue
		}
		seen[s.Project] = true
		projects = append(projects, p)
	}
	if out.Started == 0 {
		return out, nil
	}
	background := a.background(ctx)
	go func() {
		for _, p := range projects {
			for _, r := range a.Coordinator.ResolveConflicts(background, p) {
				if r.Err != nil {
					slog.Warn(fmt.Sprintf("admin: resolve conflict %s in %s: %v", r.Artifact, p.ID, r.Err), "artifact", r.Artifact, "project", p.ID)
				}
			}
		}
	}()
	return out, nil
}

// ResolveConflictWithAgent hands one conflict, named by the artifact stuck
// behind it, to an agent.
func (a *Service) ResolveConflictWithAgent(ctx context.Context, artifactID string) (consoleapi.ResolveConflictsResult, error) {
	var out consoleapi.ResolveConflictsResult
	stuck, p, err := a.conflict(ctx, artifactID)
	if err != nil {
		return out, err
	}
	if !stuck.Resolvable() {
		out.Skipped = []string{stuck.Artifact}
		return out, nil
	}
	out.Started = 1
	background := a.background(ctx)
	go func() {
		for _, r := range a.Coordinator.ResolveOneConflict(background, p, stuck.Artifact) {
			if r.Err != nil {
				slog.Warn(fmt.Sprintf("admin: resolve conflict %s in %s: %v", r.Artifact, p.ID, r.Err), "artifact", r.Artifact, "project", p.ID)
			}
		}
	}()
	return out, nil
}

// ConflictFile reads one of a conflict's files as git left it: both sides
// with the markers between them, which is what a person edits.
func (a *Service) ConflictFile(ctx context.Context, artifactID, path string) (consoleapi.ConflictFileView, error) {
	stuck, p, err := a.conflict(ctx, artifactID)
	if err != nil {
		return consoleapi.ConflictFileView{}, err
	}
	if stuck.Marked == "" {
		return consoleapi.ConflictFileView{}, errors.New("this conflict left no half-merged tree to read")
	}
	if err := a.Artifacts.BringHome(ctx, p, stuck.Marked); err != nil {
		return consoleapi.ConflictFileView{}, err
	}
	text, size, binary, truncated, err := a.Artifacts.File(ctx, p.ID, stuck.Marked, path)
	if err != nil {
		return consoleapi.ConflictFileView{}, err
	}
	return consoleapi.ConflictFileView{
		Artifact: stuck.Artifact, Project: p.ID, Path: path, Text: text,
		Size: size, Binary: binary, Truncated: truncated,
	}, nil
}

// ResolveConflictByHand takes a person's own resolution of every file and
// lands it, the same way an agent's resolution lands.
func (a *Service) ResolveConflictByHand(ctx context.Context, artifactID string, edits []artifact.Edit) error {
	stuck, p, err := a.conflict(ctx, artifactID)
	if err != nil {
		return err
	}
	_, err = a.Artifacts.ResolveByHand(ctx, p, stuck, edits, "console")
	return err
}

// RetryConflict sends a result that stopped with no merge to work on — a
// write refused because the working tree changed under it, or one into a
// nested repository — to land again on the next pass. Nothing automatic
// retries such a result; its owner does, after dealing with the cause.
func (a *Service) RetryConflict(ctx context.Context, artifactID, landing string) error {
	stuck, p, err := a.conflict(ctx, artifactID)
	if err != nil {
		return err
	}
	return a.Artifacts.Unblock(ctx, p.ID, stuck.Artifact, landing)
}

// conflict finds a stuck result and the project it belongs to.
func (a *Service) conflict(ctx context.Context, artifactID string) (artifact.Stuck, project.Project, error) {
	if a.Artifacts == nil || a.Coordinator == nil || a.Projects == nil {
		return artifact.Stuck{}, project.Project{}, fmt.Errorf("conflict resolution is not wired")
	}
	stuck, ok, err := a.Artifacts.StuckOne(ctx, artifactID)
	if err != nil {
		return artifact.Stuck{}, project.Project{}, err
	}
	if !ok {
		return artifact.Stuck{}, project.Project{}, fmt.Errorf("%w: nothing is stuck on a conflict for %s", artifact.ErrNotBlocked, artifactID)
	}
	p, ok, err := a.Projects.Get(ctx, stuck.Project)
	if err != nil {
		return artifact.Stuck{}, project.Project{}, err
	}
	if !ok {
		return artifact.Stuck{}, project.Project{}, fmt.Errorf("no project %s", stuck.Project)
	}
	return stuck, p, nil
}

// background is the context a resolution runs under: the browser is not
// going to hold a connection open for a plan, and cancelling the fetch
// must not cancel a resolution that is already editing files.
func (a *Service) background(ctx context.Context) context.Context {
	if a.Lifetime != nil {
		return a.Lifetime
	}
	return context.WithoutCancel(ctx)
}

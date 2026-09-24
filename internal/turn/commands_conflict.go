// The /resolve command and the automatic resolution behind it: handing an
// agent the half-merged workspace of a landing that stopped at a merge
// conflict, so the queue moves again without a person editing markers.

package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

// Resolution is what came of asking an agent to resolve one conflict.
type Resolution struct {
	Artifact string
	Landing  string
	Paths    []string
	TaskID   string
	Agent    string
	Err      error
}

// resolveCmd is `/resolve`: work through the conversation project's queued
// results that are stuck on a merge conflict. With no argument it takes
// them all; with one it takes the artifact whose id starts with it.
func (c commands) resolveCmd(ctx context.Context, req Request, rest string) (Result, error) {
	ctx = agentexec.WithProgress(ctx, req.OnProgress)
	title := c.text.T(i18n.CardResolve)
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		var user UserError
		if errors.As(err, &user) {
			return Result{Title: title, Text: user.Text}, nil
		}
		return Result{Title: title, Text: c.text.T(i18n.ResolveFailed, err)}, nil
	}
	p, ok, err := c.projects.Get(ctx, binding.ProjectID)
	if err != nil || !ok {
		return Result{Title: title, Text: c.text.T(i18n.ResolveFailed, fmt.Errorf("project %s", binding.ProjectID))}, nil
	}
	stuck, err := c.artifacts.Stuck(ctx, p.ID)
	if err != nil {
		return Result{Title: title, Text: c.text.T(i18n.ResolveFailed, err)}, nil
	}
	if want := strings.TrimSpace(rest); want != "" {
		var picked []artifact.Stuck
		for _, s := range stuck {
			if strings.HasPrefix(s.Artifact, want) {
				picked = append(picked, s)
			}
		}
		if len(picked) == 0 {
			return Result{Title: title, Text: c.text.T(i18n.ResolveUnknown, want)}, nil
		}
		stuck = picked
	}
	if len(stuck) == 0 {
		return Result{Title: title, Text: c.text.T(i18n.ResolveNothing, p.ID)}, nil
	}
	var b strings.Builder
	for _, s := range c.resolveAll(ctx, p, stuck, req, false) {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch {
		case s.Err != nil:
			b.WriteString(c.text.T(i18n.ResolveStopped, shortID(s.Artifact), s.Err))
		default:
			b.WriteString(c.text.T(i18n.ResolveDone, shortID(s.Artifact), s.Agent, strings.Join(s.Paths, ", ")))
		}
	}
	return Result{Title: title, Text: b.String()}, nil
}

// AutoResolveConflicts is what the sweeper calls: the same work with
// nobody asking for it, and nothing at all when the landing policy leaves
// conflicts to a person.
func (c *Coordinator) AutoResolveConflicts(ctx context.Context, p project.Project) []Resolution {
	if !c.autoResolves() {
		return nil
	}
	return c.ResolveConflicts(ctx, p)
}

// ResolveConflicts works through every queued result of the project that
// is stuck on a merge conflict. It costs nothing when none is.
func (c *Coordinator) ResolveConflicts(ctx context.Context, p project.Project) []Resolution {
	stuck, err := c.artifacts.Stuck(ctx, p.ID)
	if err != nil || len(stuck) == 0 {
		return nil
	}
	return c.resolveAll(ctx, p, stuck, Request{Locale: string(c.text.Locale())}, true)
}

// ResolveOneConflict hands a single stuck result to an agent, wherever it
// is queued. The console lists conflicts across every project, so an
// action taken on one of them names the artifact rather than the project
// it happens to belong to.
func (c *Coordinator) ResolveOneConflict(ctx context.Context, p project.Project, artifactID string) []Resolution {
	stuck, err := c.artifacts.Stuck(ctx, p.ID)
	if err != nil {
		return nil
	}
	var picked []artifact.Stuck
	for _, s := range stuck {
		if s.Artifact == artifactID {
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	return c.resolveAll(ctx, p, picked, Request{Locale: string(c.text.Locale())}, false)
}

// resolveAll takes the conflicts one at a time. Each resolution moves the
// canonical name, so a later one in the same pass merges onto what the
// earlier one produced rather than onto a snapshot that is already stale.
// auto skips a conflict that has already had its automatic try: whatever
// stopped it would stop it again, and each try costs an agent run. It also
// skips one with no conflicted tree to work in, which only the owner can
// settle; failing on it every pass would only repeat the same report.
func (c *Coordinator) resolveAll(ctx context.Context, p project.Project, stuck []artifact.Stuck, req Request, auto bool) []Resolution {
	var out []Resolution
	for _, s := range stuck {
		if auto && (s.Attempt != "" || !s.Resolvable()) {
			continue
		}
		if !c.claimResolution(p.ID, s.Artifact) {
			continue
		}
		result := c.resolveConflict(ctx, p, s, req)
		c.releaseResolution(p.ID, s.Artifact)
		out = append(out, result)
	}
	return out
}

// claimResolution keeps one conflict from being worked twice at once: a
// resolution runs for minutes, and the sweeper comes round every thirty
// seconds while the canonical name has not moved yet.
func (c *Coordinator) claimResolution(projectID, artifactID string) bool {
	key := projectID + "/" + artifactID
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolving == nil {
		c.resolving = map[string]bool{}
	}
	if c.resolving[key] {
		return false
	}
	c.resolving[key] = true
	return true
}

func (c *Coordinator) releaseResolution(projectID, artifactID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.resolving, projectID+"/"+artifactID)
}

// resolveConflict runs one fixed step: the agent starts in the conflicted
// tree git already built — both sides' edits with markers between them —
// and the step counts only when a command finds no markers left. What it
// produces lands the ordinary way, which moves the canonical name and lets
// the original queued result merge cleanly on the next pass.
func (c *Coordinator) resolveConflict(ctx context.Context, p project.Project, stuck artifact.Stuck, req Request) Resolution {
	out := Resolution{Artifact: stuck.Artifact, Landing: stuck.Landing, Paths: stuck.Paths}
	if !stuck.Resolvable() {
		out.Err = errors.New(c.text.T(i18n.ResolveNoTree))
		return out
	}
	agentID, err := c.conflictAgent(ctx, p)
	if err != nil {
		out.Err = err
		return out
	}
	out.Agent = agentID
	goal := c.text.T(i18n.ResolveGoal, p.ID, shortID(stuck.Artifact), strings.Join(stuck.Paths, "\n- "))
	tracked, err := c.openPlanTask(req, "resolve merge conflict "+shortID(stuck.Artifact)+" in "+p.ID, p.ID)
	if err != nil {
		out.Err = err
		return out
	}
	out.TaskID = tracked.ID
	// Written before the work starts, so a crash mid-resolution does not
	// come back to a fresh attempt every thirty seconds.
	if err := c.artifacts.Attempting(ctx, p.ID, stuck.Artifact, tracked.ID); err != nil {
		slog.Warn(fmt.Sprintf("turn: record resolution attempt for %s: %v", shortID(stuck.Artifact), err), "artifact", stuck.Artifact, "project", p.ID)
	}
	out.Err = c.runResolution(ctx, p, stuck, tracked, agentID, goal)
	if out.Err != nil {
		// The run is over either way, and an automatic resolution has no
		// conversation behind it: plan recovery skips a task with no
		// anchor, so nothing else would ever close this one.
		c.closePlanTask(tracked.ID, out.Err)
	}
	return out
}

// runResolution is the part that can fail after the task exists, kept
// apart so every way out of it goes through one close.
func (c *Coordinator) runResolution(ctx context.Context, p project.Project, stuck artifact.Stuck, tracked task.Task, agentID, goal string) error {
	scope, err := c.executions.Begin(ctx, execution.Key{TaskID: tracked.ID, InstanceID: "resolve/" + tracked.ID})
	if err != nil {
		return err
	}
	defer scope.Finish(nil)
	ctx = scope.Context()
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	stored, err := c.plans.Create(plan.Plan{
		TaskID: tracked.ID, ProjectID: p.ID, Goal: goal,
		By: "resolve", Because: "merge conflict in landing " + stuck.Landing, Fixed: true,
		Base: stuck.Marked,
		Steps: []plan.Step{{
			ID: "resolve", Goal: goal, Agent: agentID,
			Verify: &plan.Verify{Kind: plan.VerifyCommand, Command: markerCheck(stuck.Paths)},
		}},
	})
	if err != nil {
		return err
	}
	if _, err := c.supervisor.Execute(ctx, stored); err != nil {
		return err
	}
	c.finishPlanTask(ctx, tracked.ID)
	return nil
}

// conflictAgent picks who resolves: an eligible agent on the project's own
// machine, since an in-place project's files are only there. An isolated
// project works in a worktree, so any eligible agent will do.
func (c *Coordinator) conflictAgent(ctx context.Context, p project.Project) (string, error) {
	if c.fleet == nil {
		return "", errors.New(c.text.T(i18n.ResolveNoAgent, p.ID))
	}
	for _, item := range c.fleet.All(ctx) {
		if !item.Eligible {
			continue
		}
		if p.Repo == project.RepoIsolated || item.Node == p.Home.Node {
			return item.Agent.ID, nil
		}
	}
	return "", errors.New(c.text.T(i18n.ResolveNoAgent, p.ID))
}

// markerCheck is the proof that the conflict is gone: every path that
// conflicted is either deleted or free of conflict markers. A step that
// says it resolved the merge but left the markers in fails here.
func markerCheck(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		quoted := shellQuote(path)
		fmt.Fprintf(&b, "if [ -e %s ] && grep -q '^<<<<<<< ' %s; then echo 'unresolved conflict markers' >&2; exit 1; fi\n", quoted, quoted)
	}
	b.WriteString("exit 0")
	return b.String()
}

// resolveHint is the line under a conflicted landing that says what to do
// about it, so "merge conflict" comes with a next step instead of a dead end.
func resolveHint(text i18n.Catalog, artifactID string) string {
	return text.T(i18n.ResolveHint, protocol.CommandResolve, shortID(artifactID))
}

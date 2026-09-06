package main

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// coordinatorInformer answers the platform MCP server's questions from
// the coordinator, in the server's own shapes.
type coordinatorInformer struct{ c *turn.Coordinator }

func (i coordinatorInformer) Context(ctx context.Context, conversationID, agentID string) (agentmcp.ContextInfo, error) {
	w, err := i.c.Where(ctx, conversationID, agentID)
	if err != nil {
		return agentmcp.ContextInfo{}, err
	}
	return agentmcp.ContextInfo{Agent: w.Agent, Node: w.Node, Harness: w.Harness, Model: w.Model, Mode: w.Mode, Project: w.Project, ProjectNode: w.ProjectNode, Level: w.Level, Repo: w.Repo,
		Workspace: w.Workspace, WorkspaceKind: w.WorkspaceKind, Why: w.Why, Task: w.Task, Turns: w.Turns, MaxTurns: w.MaxTurns, Elapsed: w.Elapsed, MaxElapsed: w.MaxElapsed, MCPServers: w.MCPServers, Skills: w.Skills}, nil
}

func (i coordinatorInformer) Projects(ctx context.Context, conversationID, agentID string) (string, error) {
	return i.c.WhereProjects(ctx, conversationID, agentID)
}

func (a *fleetAdmin) SetTaskMeta(ctx context.Context, taskID string, patch consoleapi.TaskMetaPatch) error {
	if a.tasks == nil {
		return errors.New("tasks are not wired")
	}
	_, err := a.tasks.SetMeta(taskID, task.MetaPatch{Title: patch.Title, Priority: patch.Priority, Labels: patch.Labels, Archived: patch.Archived})
	return err
}

// ProjectOf is the project a conversation works in, for the console's
// quote boundary.
func (a *fleetAdmin) ProjectOf(ctx context.Context, conversation string) string {
	if a.coordinator == nil {
		return ""
	}
	return a.coordinator.ProjectOf(ctx, conversation)
}

// Selectors are what an agent offers in a thread, with what is chosen.
func (a *fleetAdmin) Selectors(ctx context.Context, conversation, agent string) (consoleapi.Selectors, error) {
	if a.coordinator == nil {
		return consoleapi.Selectors{}, errors.New("coordinator is not wired")
	}
	sel, err := a.coordinator.Selectors(ctx, conversation, agent)
	if err != nil {
		return consoleapi.Selectors{}, err
	}
	return consoleapi.Selectors{Model: sel.Model, Models: sel.Models, Options: sel.Options, Preferred: a.coordinator.Preferences(conversation, agent)}, nil
}

// SetPreferences records the owner's choices for an agent in a thread.
func (a *fleetAdmin) SetPreferences(ctx context.Context, conversation, agent string, patch map[string]string) error {
	if a.coordinator == nil {
		return errors.New("coordinator is not wired")
	}
	return a.coordinator.SetPreferences(ctx, conversation, agent, patch)
}

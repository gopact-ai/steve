package turn

import (
	"context"
	"errors"
	"log"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

// bindingFor returns the conversation's project binding, making the
// default one on first use. The default is recorded like any other
// binding — version 1, by "default" — so a later switch is a visible
// change from something, not from nothing.
func (c *Coordinator) bindingFor(ctx context.Context, req Request) (project.Binding, error) {
	if c.projects == nil {
		return project.Binding{}, UserError{Text: c.text.T(i18n.ProjectsDisabled)}
	}
	binding, ok, err := c.projects.Binding(ctx, req.ConversationID)
	if err != nil {
		return project.Binding{}, err
	}
	if ok {
		return binding, nil
	}
	id := c.defaultProject
	if c.homeProject != "" && injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) == home.ModeOwner {
		id = c.homeProject
	}
	if id == "" {
		return project.Binding{}, UserError{Text: c.text.T(i18n.ProjectUnbound, protocol.CommandProject)}
	}
	binding, err = c.projects.Bind(ctx, req.ConversationID, id, "default")
	if err != nil {
		return project.Binding{}, err
	}
	log.Printf("turn: conversation %s bound to project %s by default", req.ConversationID, id)
	return binding, nil
}

// resolveWorkspace is where an interactive turn learns its directory: the
// conversation's project, materialised on the agent's node. An agent that
// cannot reach the project's canonical workspace is told so in terms of
// places, with the two ways out.
func (c *Coordinator) resolveWorkspace(ctx context.Context, req Request, selected agent.Agent) (project.Binding, project.Workspace, error) {
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	if err := checkScheduledProject(req.ExpectedProject, binding.ProjectID); err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	if err := c.require(ctx, binding.ProjectID, req.SenderOpenID, project.RoleWrite); err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	if c.tasks != nil {
		if tracked, ok := c.tasks.RecoveryOn(req.ConversationID, selected.ID, req.Origin); ok {
			recovered := tracked.RecoveryWorkspace
			if recovered.ProjectID == binding.ProjectID && recovered.NodeID == selected.Node && recovered.HarnessID == selected.Harness {
				return binding, project.Workspace{ID: recovered.ID, Project: recovered.ProjectID, Node: recovered.NodeID, Path: recovered.Path, Kind: project.KindWorktree, Base: recovered.Base}, nil
			}
		}
	}
	workspace, err := c.projects.Materialize(ctx, project.Request{Project: binding.ProjectID, Node: selected.Node})
	if err != nil {
		var notHome project.NotHomeError
		if errors.As(err, &notHome) {
			return project.Binding{}, project.Workspace{}, UserError{Text: c.text.T(i18n.ProjectNotHome,
				binding.ProjectID, notHome.PlaceList(), selected.ID, placeLabel(selected.Node), placeLabel(selected.Node), protocol.CommandProject)}
		}
		return project.Binding{}, project.Workspace{}, err
	}
	return binding, workspace, nil
}

// sessionDrifted says whether a saved session was opened under a different
// binding than the one in force now. A session from before bindings were
// recorded is judged on its directory alone.
func sessionDrifted(saved state.Session, binding project.Binding, workspace string) bool {
	if saved.Workspace != "" && saved.Workspace != workspace {
		return true
	}
	if saved.ProjectID == "" {
		return false
	}
	return saved.ProjectID != binding.ProjectID || saved.ProjectVersion != binding.Version
}

func (c *Coordinator) isActive(conversationID, agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, active := c.active[sessionKey(conversationID, agentID)]
	return active
}

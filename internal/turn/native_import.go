package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
)

// ImportNativeSession binds history without creating a task, process or prompt.
// The first subsequent user message follows normal admission and native open.
func (c *Coordinator) ImportNativeSession(ctx context.Context, conversation, projectID string, selected agent.Agent, ref nativehistory.Reference) error {
	if c.projects == nil || c.store == nil {
		return errors.New("native import requires project and conversation storage")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	key := sessionKey(conversation, selected.ID)
	if c.skillsLock > 0 || c.cancels[key] != nil {
		c.mu.Unlock()
		return errors.New("conversation is busy; retry its original import")
	}
	c.cancels[key] = &turnEntry{cancel: cancel, done: make(chan struct{})}
	c.mu.Unlock()
	defer c.clearActive(conversation, selected.ID)
	if err := c.require(ctx, projectID, c.ownerOpenID, project.RoleWrite); err != nil {
		return err
	}
	workspace, err := c.projects.Materialize(ctx, project.Request{Project: projectID, Node: selected.Node})
	if err != nil {
		return err
	}
	if err := ref.Validate(selected.Harness, workspace.Path); err != nil {
		return err
	}
	binding, exists, err := c.projects.Binding(ctx, conversation)
	if err != nil {
		return err
	}
	if exists && binding.ProjectID != projectID {
		return errors.New("native import command is already bound to another project")
	}
	if !exists {
		binding, err = c.projects.Bind(ctx, conversation, projectID, "native-import")
		if err != nil {
			return err
		}
	}
	return c.store.InstallNativeSession(state.Session{ConversationID: conversation, AgentID: selected.ID, HarnessID: selected.Harness, NodeID: selected.Node,
		Workspace: workspace.Path, ProjectID: projectID, ProjectVersion: binding.Version, NativeImport: ref.Clone()})
}

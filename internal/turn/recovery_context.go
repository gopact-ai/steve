package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
)

// The context bar describes the same fixed location as the next input, without
// materializing it or moving the conversation's project binding.
func (c *Coordinator) describeRecoveryWorkspace(ctx context.Context, p *project.Project, out *Context) error {
	if p == nil {
		return nil
	}
	r, found, err := c.attempts.RecoveryForProject(ctx, p.ID)
	if err != nil || !found {
		return err
	}
	for i := range out.Agents {
		choice := &out.Agents[i]
		if !choice.Ready || choice.Place != nil && choice.Place.Kind == string(project.KindCopy) {
			continue
		}
		if r.Workspace.Path == "" {
			choice.Place = nil
			choice.Because = c.text.T(i18n.RecoveryCopyPreparing, p.ID)
			continue
		}
		if choice.Node != nodewire.Place(r.Workspace.Node) {
			choice.Place = nil
			choice.Usable = false
			choice.Because = c.text.T(i18n.RecoveryCopyNode, p.ID, nodewire.Place(r.Workspace.Node))
			continue
		}
		choice.Place = &Placement{Workspace: r.Workspace.ID, Kind: string(project.KindWorktree), Node: nodewire.Place(r.Workspace.Node)}
	}
	return nil
}

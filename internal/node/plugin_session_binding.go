package node

import (
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func validatePluginSessionOpen(req nodewire.SessionRequest) error {
	if req.Action != nodewire.SessionActionOpen {
		return nil
	}
	if req.Plugin == nil {
		if req.Binding.PluginRuntimeID != "" {
			return plugins.ErrInvalid
		}
		return nil
	}
	if req.Plugin.Validate() != nil || req.Plugin.ID != req.Binding.PluginRuntimeID || req.Plugin.Selection.Project != req.Binding.ProjectID || req.Plugin.Selection.Node != req.Binding.NodeID || req.Plugin.Selection.Harness != req.Harness {
		return plugins.ErrInvalid
	}
	return nil
}

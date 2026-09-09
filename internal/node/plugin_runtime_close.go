package node

import (
	"context"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (s *Server) closePluginRuntime(ctx context.Context, req nodewire.PluginRequest) error {
	if req.Runtime == nil || req.Runtime.Validate() != nil || req.Runtime.Selection.Node != s.conf().Name {
		return plugins.ErrInvalid
	}
	if s.sessions != nil {
		s.sessions.mu.Lock()
		var sessions []*ownedSession
		for _, one := range s.sessions.sessions {
			sessions = append(sessions, one)
		}
		s.sessions.mu.Unlock()
		var selected []*ownedSession
		for _, one := range sessions {
			one.mu.Lock()
			matches := one.record.State.Plugin != nil && one.record.State.Plugin.ID == req.Runtime.ID
			one.mu.Unlock()
			if matches {
				selected = append(selected, one)
			}
		}
		for _, one := range selected {
			one.mu.Lock()
			state := one.copyLocked().State
			one.mu.Unlock()
			request := nodewire.SessionRequest{Action: nodewire.SessionActionClose, Authority: req.Authority, Binding: state.Binding, ID: state.ID}
			// The plugin stream already checked coordinator authority and live
			// attempts. stop still checks session identity and refuses active work.
			if _, err := one.stop(ctx, request); err != nil {
				return err
			}
		}
	}
	info, err := s.pluginStore().RuntimeInfo(req.Runtime.ID)
	if err != nil {
		return err
	}
	for _, usage := range info.Uses {
		if !usage.Stopped {
			return plugins.ErrRuntimeBusy
		}
	}
	return s.pluginRuntimePool().Drop(req.Runtime.ID)
}

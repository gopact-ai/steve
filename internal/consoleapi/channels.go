package consoleapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/config"
)

type ChannelsView struct {
	Revision       string                 `json:"revision"`
	Desired        config.ChannelSettings `json:"desired"`
	Effective      config.ChannelSettings `json:"effective"`
	PendingRestart bool                   `json:"pending_restart"`
	ApplyMode      string                 `json:"apply_mode"`
	Warning        string                 `json:"warning,omitempty"`
	RuntimeError   string                 `json:"runtime_error,omitempty"`
}

type ChannelsUpdate struct {
	BaseRevision string              `json:"base_revision"`
	Channels     config.ChannelPatch `json:"channels"`
}

type ChannelsService interface {
	Channels(context.Context) (ChannelsView, error)
	UpdateChannels(context.Context, ChannelsUpdate) (ChannelsView, error)
}

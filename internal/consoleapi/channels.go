package consoleapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/channelsettings"
)

type ChannelsView struct {
	Revision       string                   `json:"revision"`
	Desired        channelsettings.Settings `json:"desired"`
	Effective      channelsettings.Settings `json:"effective"`
	PendingRestart bool                     `json:"pending_restart"`
	ApplyMode      string                   `json:"apply_mode"`
	LiveFields     []string                 `json:"live_fields,omitempty"`
	Warning        string                   `json:"warning,omitempty"`
	RuntimeError   string                   `json:"runtime_error,omitempty"`
}

type ChannelsUpdate struct {
	BaseRevision string                `json:"base_revision"`
	Channels     channelsettings.Patch `json:"channels"`
}

type ChannelsService interface {
	Channels(context.Context) (ChannelsView, error)
	UpdateChannels(context.Context, ChannelsUpdate) (ChannelsView, error)
}

package consoleapi

import (
	"context"
	"time"

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
	StartupRetry   *ChannelStartupRetry     `json:"startup_retry,omitempty"`
}

// ChannelStartupRetry reports a channel whose startup failed with an error
// that may pass, and when it is tried again.
type ChannelStartupRetry struct {
	Attempts  int       `json:"attempts"`
	NextAt    time.Time `json:"next_at"`
	LastError string    `json:"last_error"`
}

type ChannelsUpdate struct {
	BaseRevision string                `json:"base_revision"`
	Channels     channelsettings.Patch `json:"channels"`
}

type ChannelsService interface {
	Channels(context.Context) (ChannelsView, error)
	UpdateChannels(context.Context, ChannelsUpdate) (ChannelsView, error)
}

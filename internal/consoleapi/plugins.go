package consoleapi

import (
	"context"

	"github.com/gopact-ai/steve/internal/plugins"
)

type PluginTargetView struct {
	Node    string                     `json:"node"`
	State   string                     `json:"state"`
	Error   string                     `json:"error,omitempty"`
	Receipt *plugins.DeploymentReceipt `json:"receipt,omitempty"`
}

type PluginInstallationView struct {
	ID           string               `json:"id"`
	Installation plugins.Installation `json:"installation"`
	Targets      []PluginTargetView   `json:"targets"`
}

type PluginsView struct {
	Warning       string                   `json:"warning,omitempty"`
	Operations    []plugins.Operation      `json:"operations"`
	Revision      string                   `json:"revision"`
	Packages      []plugins.PackageRecord  `json:"packages"`
	Installations []PluginInstallationView `json:"installations"`
}

type PluginPreview struct {
	Manifest plugins.Manifest `json:"manifest"`
	Digest   string           `json:"digest"`
	Source   plugins.Source   `json:"source"`
}

type PluginImportRequest struct {
	CommandID string         `json:"command_id"`
	Project   string         `json:"project"`
	Digest    string         `json:"digest"`
	Source    plugins.Source `json:"source"`
}

type PluginUpdateRequest struct {
	BaseRevision string               `json:"base_revision"`
	Installation plugins.Installation `json:"installation"`
}

type PluginsService interface {
	Plugins(context.Context) (PluginsView, error)
	PreviewPlugin(context.Context, plugins.Source) (PluginPreview, error)
	ImportPlugin(context.Context, PluginImportRequest) (plugins.PackageRecord, error)
	UpdatePlugin(context.Context, string, PluginUpdateRequest) (PluginsView, error)
	PreparePlugin(context.Context, string) (PluginInstallationView, error)
	PluginSecrets(context.Context, string) ([]plugins.SecretInfo, error)
}

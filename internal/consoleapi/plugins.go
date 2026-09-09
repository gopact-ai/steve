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

type PluginResourceView struct {
	Installation string   `json:"installation"`
	PackageID    string   `json:"package_id"`
	Version      string   `json:"version"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Projects     []string `json:"projects"`
}

type PluginInstallationView struct {
	ID           string               `json:"id"`
	Installation plugins.Installation `json:"installation"`
	Targets      []PluginTargetView   `json:"targets"`
}

type PluginsView struct {
	Agents        []PluginAgentView        `json:"agents"`
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

type PluginPresetRequest struct {
	Adopt        *plugins.Adoption `json:"adopt,omitempty"`
	CommandID    string            `json:"command_id"`
	BaseRevision string            `json:"base_revision"`
	AgentID      string            `json:"agent_id"`
	Node         string            `json:"node"`
	Preset       string            `json:"preset"`
	Digest       string            `json:"digest"`
	Model        *string           `json:"model,omitempty"`
	SystemPrompt *string           `json:"system_prompt,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
}

type PluginAgentView struct {
	Skills       []string             `json:"skills,omitempty"`
	MCPServers   []string             `json:"mcp_servers,omitempty"`
	ID           string               `json:"id"`
	Node         string               `json:"node"`
	Harness      string               `json:"harness"`
	Model        string               `json:"model,omitempty"`
	Options      map[string]string    `json:"options,omitempty"`
	SystemPrompt string               `json:"system_prompt,omitempty"`
	Origin       *plugins.AgentOrigin `json:"origin,omitempty"`
}

type PluginPresetPreview struct {
	Revision  string           `json:"revision"`
	Proposed  PluginAgentView  `json:"proposed"`
	Existing  *PluginAgentView `json:"existing,omitempty"`
	Preserved []string         `json:"preserved"`
	Warning   string           `json:"warning,omitempty"`
}

type PluginPresetService interface {
	PreviewPluginPreset(context.Context, string, PluginPresetRequest) (PluginPresetPreview, error)
	ApplyPluginPreset(context.Context, string, PluginPresetRequest) (PluginPresetPreview, error)
}

type PluginRuntimeReference struct {
	Kind    string             `json:"kind"`
	Owner   string             `json:"owner"`
	Runtime plugins.RuntimeRef `json:"runtime"`
}

type PluginUsageView struct {
	References []PluginRuntimeReference `json:"references"`
	Runtimes   []plugins.RuntimeInfo    `json:"runtimes"`
	Errors     map[string]string        `json:"errors"`
}

type PluginRemoveRequest struct {
	BaseRevision string `json:"base_revision"`
}

type PluginRemovalService interface {
	PluginUsage(context.Context, string) (PluginUsageView, error)
	RemovePlugin(context.Context, string, PluginRemoveRequest) (PluginsView, error)
}

type PluginRuntimeCloseService interface {
	ClosePluginRuntime(context.Context, string, string) (PluginUsageView, error)
}

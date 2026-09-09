package nodewire

import "github.com/gopact-ai/steve/internal/plugins"

const (
	FeaturePlugins = "plugin_packages.v1"
	StreamPlugins  = "plugins"
	PluginMaxBytes = 48 << 20
)

type PluginAction string

const (
	PluginPrepare PluginAction = "prepare"
	PluginInspect PluginAction = "inspect"
	PluginSecrets PluginAction = "secrets"
)

type PluginRequest struct {
	Action     PluginAction       `json:"action"`
	Authority  SessionAuthority   `json:"authority"`
	Node       string             `json:"node"`
	Deployment plugins.Deployment `json:"deployment"`
	Bundle     []byte             `json:"bundle,omitempty"`
}

type PluginReply struct {
	Authorize bool                       `json:"authorize,omitempty"`
	Receipt   *plugins.DeploymentReceipt `json:"receipt,omitempty"`
	Secrets   []plugins.SecretInfo       `json:"secrets,omitempty"`
	ErrorCode string                     `json:"error_code,omitempty"`
	Error     string                     `json:"error,omitempty"`
}

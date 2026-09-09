package nodewire

import (
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/plugins"
)

const (
	FeaturePlugins        = "plugin_packages.v1"
	FeaturePluginRuntimes = "plugin_runtimes.v1"
	StreamPlugins         = "plugins"
	PluginMaxBytes        = 48 << 20
)

type PluginAction string

const (
	PluginPrepare        PluginAction = "prepare"
	PluginRuntimePrepare PluginAction = "runtime-prepare"
	PluginRuntimeInspect PluginAction = "runtime-inspect"
	PluginInspect        PluginAction = "inspect"
	PluginSecrets        PluginAction = "secrets"
)

type PluginRequest struct {
	Permission string              `json:"permission,omitempty"`
	CommandID  string              `json:"command_id,omitempty"`
	Selection  *plugins.Selection  `json:"selection,omitempty"`
	Runtime    *plugins.RuntimeRef `json:"runtime,omitempty"`
	Action     PluginAction        `json:"action"`
	Authority  SessionAuthority    `json:"authority"`
	Node       string              `json:"node"`
	Deployment plugins.Deployment  `json:"deployment"`
	Bundle     []byte              `json:"bundle,omitempty"`
}

type PluginReply struct {
	Permission   string                     `json:"permission,omitempty"`
	Runtime      *plugins.RuntimeRef        `json:"runtime,omitempty"`
	Servers      []acp.MCPServer            `json:"servers,omitempty"`
	Instructions string                     `json:"instructions,omitempty"`
	Authorize    bool                       `json:"authorize,omitempty"`
	Receipt      *plugins.DeploymentReceipt `json:"receipt,omitempty"`
	Secrets      []plugins.SecretInfo       `json:"secrets,omitempty"`
	ErrorCode    string                     `json:"error_code,omitempty"`
	Error        string                     `json:"error,omitempty"`
}

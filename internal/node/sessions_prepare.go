package node

import (
	"context"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

func (s *SessionService) prepareSessionHost(ctx context.Context, id, resumeRuntime string, req *nodewire.SessionRequest, spec HarnessSpec, broker *permission.Broker) (acphost.Config, string, error) {
	cfg := s.hostConfig(req.Harness, spec, broker)
	instructions := ""
	if req.NativeImport != nil {
		if req.NativeImport.ID != req.Binding.NativeImportID {
			return cfg, "", sessionError("invalid", "native import differs from committed execution")
		}
		if err := req.NativeImport.Validate(req.Harness, req.Workdir); err != nil {
			return cfg, "", err
		}
	} else if req.Binding.NativeImportID != "" {
		return cfg, "", sessionError("invalid", "committed native import is missing")
	}
	if req.Plugin != nil {
		if req.Plugin.ID != req.Binding.PluginRuntimeID || req.Plugin.Selection.Project != req.Binding.ProjectID || req.Plugin.Selection.Harness != req.Harness || req.Plugin.Selection.Node != req.Binding.NodeID {
			return cfg, "", plugins.ErrInvalid
		}
		prepared, err := s.server.pluginRuntimePool().Load(ctx, *req.Plugin)
		if err != nil {
			return cfg, "", err
		}
		instructions = prepared.Instructions
		cfg = acphost.Config{NoRestart: true, Command: prepared.Config.Command, Args: prepared.Config.Args, Env: prepared.Config.Env, ProcessDir: prepared.Config.ProcessDir, Permission: broker}
		req.MCPServers = append(append([]acp.MCPServer(nil), req.MCPServers...), prepared.Servers...)
	} else if req.Binding.PluginRuntimeID != "" {
		return cfg, "", plugins.ErrInvalid
	}
	if req.NativeImport != nil {
		prepare := steveruntime.PrepareNativeHistory
		if resumeRuntime != "" {
			id, prepare = resumeRuntime, steveruntime.ResumeNativeHistory
		}
		isolated, err := prepare(ctx, s.server.conf().StateDir, id, *req.NativeImport, harness.Config{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, ProcessDir: cfg.ProcessDir})
		if err != nil {
			return cfg, "", err
		}
		cfg.Env = isolated.Env
	}
	return cfg, instructions, nil
}

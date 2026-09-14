package node

import (
	"context"
	"os"
	"path/filepath"

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

// prepareOwnedSession reserves durable context and runtime use before any native process starts.
// The caller holds s.mu across source selection and this reservation.
func (s *SessionService) prepareOwnedSession(ctx context.Context, id, hash string, req *nodewire.SessionRequest, spec HarnessSpec, broker *permission.Broker, source *sessionRecord) (*ownedSession, acphost.Config, error) {
	configHash := sessionConfigHash(*req)
	runtimeID := ""
	if source != nil {
		runtimeID = source.RuntimeSession
		if runtimeID == "" {
			runtimeID = source.State.ID
		}
	}
	hostCfg, pluginInstructions, err := s.prepareSessionHost(ctx, id, runtimeID, req, spec, broker)
	if err != nil {
		return nil, acphost.Config{}, err
	}
	host := acphost.New(hostCfg)
	one := &ownedSession{pluginInstructions: pluginInstructions, service: s, host: host, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	one.record = sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenHash: hash, ConfigHash: configHash, State: nodewire.SessionState{ID: id, NativeImport: req.NativeImport.Clone(), Plugin: req.Plugin.Clone(), Binding: req.Binding, Harness: req.Harness, State: nodewire.SessionOpening, Questions: []nodewire.SessionQuestion{}}, CommandHashes: map[string]string{}, Commands: map[string]nodewire.SessionCommand{}}
	if source != nil {
		one.record.UpstreamID, one.record.ResumedFrom, one.record.RuntimeSession = source.UpstreamID, source.State.ID, runtimeID
		// Publish the source claim before the destination can start. A crash
		// here permits only this exact open to finish reserving the destination.
		source.ResumeTarget = id
		archive := &ownedSession{service: s, record: *source, changed: make(chan struct{})}
		if err := archive.commitLocked(*source); err != nil {
			host.Close()
			return nil, acphost.Config{}, err
		}
	}
	if err := one.commitLocked(one.record); err != nil {
		// Save can fail after publishing its record. Only remove preparation
		// when durable absence is confirmed; uncertainty retains the history.
		if req.NativeImport != nil && source == nil {
			if _, exists, readErr := s.readRecord(id); readErr == nil && !exists {
				_ = os.RemoveAll(filepath.Join(s.server.conf().StateDir, "native-runtimes", id))
			}
		}
		host.Close()
		return nil, acphost.Config{}, err
	}
	if req.Plugin != nil {
		if err := s.server.pluginStore().BeginRuntimeUse(ctx, *req.Plugin, "session/"+id, "session"); err != nil {
			host.Close()
			return nil, acphost.Config{}, err
		}
	}
	return one, hostCfg, nil
}

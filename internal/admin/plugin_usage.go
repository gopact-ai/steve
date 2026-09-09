package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/state"
)

func (s *PluginService) runtimeBelongs(ctx context.Context, id string, ref plugins.RuntimeRef) (bool, error) {
	for _, hash := range ref.Selection.Deployments {
		receipt, err := s.Library.Deployment(ctx, hash)
		if err != nil {
			return false, err
		}
		if receipt.Deployment.Installation == id {
			return true, nil
		}
	}
	return false, nil
}

func (s *PluginService) PluginUsage(ctx context.Context, id string) (consoleapi.PluginUsageView, error) {
	_, items := s.snapshot()
	item, exists := items[id]
	if !exists {
		return consoleapi.PluginUsageView{}, plugins.ErrUnavailable
	}
	out := consoleapi.PluginUsageView{References: []consoleapi.PluginRuntimeReference{}, Runtimes: []plugins.RuntimeInfo{}, Errors: map[string]string{}}
	sessions, err := state.PluginReferences(s.Library.Ledger.Document("state"))
	if err != nil {
		return out, err
	}
	nodes, err := s.installationNodes(ctx, id, item)
	if err != nil {
		return out, err
	}
	for _, reference := range sessions {
		belongs, err := s.runtimeBelongs(ctx, id, reference.Runtime)
		if err != nil {
			return out, err
		}
		if !belongs {
			continue
		}
		kind := "session"
		if reference.Archived {
			kind = "archive"
		}
		out.References = append(out.References, consoleapi.PluginRuntimeReference{Kind: kind, Owner: reference.Conversation + "/" + reference.Agent, Runtime: reference.Runtime})
		nodes[reference.Runtime.Selection.Node] = true
	}
	active, err := attempt.New(s.Library.Ledger).Live(ctx)
	if err != nil {
		return out, err
	}
	for _, record := range active {
		if record.PluginRuntime == nil {
			reservation, found, err := s.Library.RuntimeReservation(ctx, record.ID)
			if err != nil {
				return out, err
			}
			if found {
				ref := plugins.RuntimeRef{ID: reservation.RuntimeID, Selection: reservation.Selection}
				belongs, err := s.runtimeBelongs(ctx, id, ref)
				if err != nil {
					return out, err
				}
				if belongs {
					out.References = append(out.References, consoleapi.PluginRuntimeReference{Kind: "preparing", Owner: record.ID, Runtime: ref})
				}
			}
			continue
		}
		belongs, err := s.runtimeBelongs(ctx, id, *record.PluginRuntime)
		if err != nil {
			return out, err
		}
		if !belongs {
			continue
		}
		out.References = append(out.References, consoleapi.PluginRuntimeReference{Kind: "attempt", Owner: record.ID, Runtime: *record.PluginRuntime.Clone()})
		nodes[record.Node] = true
	}
	names := make([]string, 0, len(nodes))
	for node := range nodes {
		names = append(names, node)
	}
	slices.Sort(names)
	for _, node := range names {
		var infos []plugins.RuntimeInfo
		if node == "" {
			infos, err = s.Local.Store.RuntimeInfos()
		} else {
			if s.Admin.Nodes == nil {
				out.Errors[node] = "node registry unavailable"
				continue
			}
			var reply nodewire.PluginReply
			reply, err = s.Admin.Nodes.Plugins(ctx, node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeList, Authority: s.Authority})
			infos = reply.Runtimes
		}
		if err != nil {
			out.Errors[node] = err.Error()
			continue
		}
		for _, info := range infos {
			belongs, err := s.runtimeBelongs(ctx, id, info.Ref)
			if err != nil {
				return out, err
			}
			if belongs {
				out.Runtimes = append(out.Runtimes, info)
				for _, record := range active {
					sum := sha256.Sum256([]byte("runtime/" + record.ID))
					if info.CommandID == hex.EncodeToString(sum[:]) && record.PluginRuntime == nil {
						out.References = append(out.References, consoleapi.PluginRuntimeReference{Kind: "preparing", Owner: record.ID, Runtime: info.Ref})
					}
				}
			}
		}
	}
	return out, nil
}

func (s *PluginService) installationNodes(ctx context.Context, id string, item plugins.Installation) (map[string]bool, error) {
	history, err := s.Library.InstallationNodes(ctx, id)
	if err != nil {
		return nil, err
	}
	nodes := map[string]bool{}
	for _, node := range history {
		nodes[node] = true
	}
	for node := range item.Targets {
		nodes[node] = true
	}
	return nodes, nil
}

func (s *PluginService) RemovePlugin(ctx context.Context, id string, req consoleapi.PluginRemoveRequest) (consoleapi.PluginsView, error) {
	s.Admin.Mu.Lock()
	defer s.Admin.Mu.Unlock()
	if s.RuntimeGate != nil {
		s.RuntimeGate.Lock()
		defer s.RuntimeGate.Unlock()
	}
	revision, items := s.snapshot()
	item, exists := items[id]
	if !exists {
		return s.Plugins(ctx)
	}
	if req.BaseRevision == "" || revision != req.BaseRevision {
		return consoleapi.PluginsView{}, consoleapi.ErrSettingsConflict
	}
	if item.Enabled {
		return consoleapi.PluginsView{}, errors.New("stop new bindings before removing this installation")
	}
	usage, err := s.PluginUsage(ctx, id)
	if err != nil {
		return consoleapi.PluginsView{}, err
	}
	if len(usage.References) > 0 || len(usage.Errors) > 0 {
		return consoleapi.PluginsView{}, plugins.ErrRuntimeBusy
	}
	for _, info := range usage.Runtimes {
		for _, use := range info.Uses {
			if !use.Stopped {
				return consoleapi.PluginsView{}, plugins.ErrRuntimeBusy
			}
		}
	}
	// Retire all unreferenced runtimes first. A concurrently prepared native
	// open then fails its persisted admission check rather than starting into
	// a directory being removed.
	for _, info := range usage.Runtimes {
		if err := s.retireRuntime(ctx, info.Ref); err != nil {
			return consoleapi.PluginsView{}, err
		}
	}
	usage, err = s.PluginUsage(ctx, id)
	if err != nil {
		return consoleapi.PluginsView{}, err
	}
	if len(usage.References) > 0 || len(usage.Errors) > 0 {
		return consoleapi.PluginsView{}, plugins.ErrRuntimeBusy
	}
	for _, info := range usage.Runtimes {
		if err := s.removeRuntime(ctx, info.Ref); err != nil {
			return consoleapi.PluginsView{}, err
		}
	}
	ConfigMu.Lock()
	if pluginRevision(s.Admin.Cfg.Plugins) != revision {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, consoleapi.ErrSettingsConflict
	}
	candidate := *s.Admin.Cfg
	candidate.Plugins = config.ClonePluginInstallations(candidate.Plugins)
	delete(candidate.Plugins, id)
	candidate.Agents = maps.Clone(candidate.Agents)
	for name, item := range candidate.Agents {
		if item.PluginOrigin != nil && item.PluginOrigin.Installation == id {
			item.PluginOrigin = nil
			candidate.Agents[name] = item
		}
	}
	catalog, err := candidate.AgentCatalog()
	if err != nil {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, err
	}
	saveErr := s.Admin.persistConfigContext(ctx, &candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, saveErr
	}
	s.Admin.Cfg.Plugins = candidate.Plugins
	s.Admin.Cfg.Agents = candidate.Agents
	if s.Admin.Catalog != nil {
		s.Admin.Catalog.Publish(catalog)
	}
	ConfigMu.Unlock()
	view, err := s.Plugins(ctx)
	if saveErr != nil {
		view.Warning = saveErr.Error()
	}
	return view, err
}

func (s *PluginService) retireRuntime(ctx context.Context, ref plugins.RuntimeRef) error {
	if ref.Selection.Node == "" {
		return s.Local.Store.RetireRuntime(ctx, ref)
	}
	_, err := s.Admin.Nodes.Plugins(ctx, ref.Selection.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeRetire, Authority: s.Authority, Runtime: &ref, Selection: &ref.Selection})
	return err
}
func (s *PluginService) removeRuntime(ctx context.Context, ref plugins.RuntimeRef) error {
	if ref.Selection.Node == "" {
		if err := s.Local.Store.CheckRuntimeRemovable(ref); err != nil {
			return err
		}
		if err := s.Local.Drop(ref.ID); err != nil {
			return err
		}
		return s.Local.Store.RemoveRuntime(ctx, ref)
	}
	_, err := s.Admin.Nodes.Plugins(ctx, ref.Selection.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeRemove, Authority: s.Authority, Runtime: &ref, Selection: &ref.Selection})
	return err
}

func (s *PluginService) ClosePluginRuntime(ctx context.Context, id, runtimeID string) (consoleapi.PluginUsageView, error) {
	s.Admin.Mu.Lock()
	defer s.Admin.Mu.Unlock()
	if s.RuntimeGate != nil {
		s.RuntimeGate.Lock()
		defer s.RuntimeGate.Unlock()
	}
	usage, err := s.PluginUsage(ctx, id)
	if err != nil {
		return usage, err
	}
	var ref *plugins.RuntimeRef
	for _, info := range usage.Runtimes {
		if info.Ref.ID == runtimeID {
			ref = info.Ref.Clone()
			break
		}
	}
	if ref == nil {
		return usage, plugins.ErrUnavailable
	}
	for _, reference := range usage.References {
		if reference.Runtime.ID == runtimeID && (reference.Kind == "attempt" || reference.Kind == "preparing") {
			return usage, plugins.ErrRuntimeBusy
		}
	}
	if s.Admin.Coordinator == nil {
		return usage, errors.New("conversation runtime manager is unavailable")
	}
	// Local and remote managers stop their known host; node-owned sessions are
	// additionally closed by the node's authoritative session service.
	if err := s.Admin.Coordinator.ForgetPluginRuntime(ctx, *ref, func(ctx context.Context) error {
		if ref.Selection.Node != "" {
			_, err := s.Admin.Nodes.Plugins(ctx, ref.Selection.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeClose, Authority: s.Authority, Runtime: ref, Selection: &ref.Selection})
			return err
		}
		info, err := s.Local.Store.RuntimeInfo(ref.ID)
		if err != nil {
			return err
		}
		for _, use := range info.Uses {
			if !use.Stopped {
				return plugins.ErrRuntimeBusy
			}
		}
		return nil
	}); err != nil {
		return usage, err
	}
	return s.PluginUsage(ctx, id)
}

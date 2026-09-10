package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/project"
)

// PluginService owns management actions, while immutable content and runtime
// resources remain with the library and each physical node.
type PluginService struct {
	RuntimeGate  *sync.RWMutex
	Admin        *Service
	Library      *plugins.Library
	Local        *node.PluginRuntimePool
	Authority    nodewire.SessionAuthority
	mu           sync.Mutex
	importMu     sync.Mutex
	observations map[string]consoleapi.PluginTargetView
}

// ErrNoCoordinator means this hub cannot deploy plugins: a node only accepts
// plugin operations from a committed cluster coordinator, the same footing
// node-owned sessions are on, and a hub started outside the clustered
// application has no such identity to present. Saying so here keeps the
// operator from reading the node's refusal as a fleet problem.
var ErrNoCoordinator = errors.New("this hub is not a cluster coordinator, so it cannot deploy plugins to nodes")

// coordinator reports whether this hub can carry out a plugin operation on
// the named machine. Its own machine needs no authority: nothing leaves the
// process. Another node checks every request against a committed
// coordinator, which a hub outside the clustered application does not have.
func (s *PluginService) coordinator(node string) error {
	if node == "" || s.Authority.ClusterID != "" {
		return nil
	}
	return ErrNoCoordinator
}

func (s *PluginService) snapshot() (string, map[string]plugins.Installation) {
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	items := config.ClonePluginInstallations(s.Admin.Cfg.Plugins)
	return pluginRevision(items), items
}

func pluginRevision(items map[string]plugins.Installation) string {
	raw, _ := json.Marshal(items)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *PluginService) Plugins(ctx context.Context) (consoleapi.PluginsView, error) {
	revision, items := s.snapshot()
	packages, err := s.Library.List(ctx)
	if err != nil {
		return consoleapi.PluginsView{}, err
	}
	operations, err := s.Library.Operations(ctx)
	if err != nil {
		return consoleapi.PluginsView{}, err
	}
	view := consoleapi.PluginsView{Operations: operations, Revision: revision, Packages: packages, Installations: []consoleapi.PluginInstallationView{}}
	view.Agents = []consoleapi.PluginAgentView{}
	ConfigMu.RLock()
	for id, item := range s.Admin.Cfg.Agents {
		view.Agents = append(view.Agents, pluginAgentView(id, item))
	}
	ConfigMu.RUnlock()
	slices.SortFunc(view.Agents, func(a, b consoleapi.PluginAgentView) int { return strings.Compare(a.ID, b.ID) })
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		view.Installations = append(view.Installations, s.installationView(ctx, id, items[id]))
	}
	return view, nil
}

func (s *PluginService) installationView(ctx context.Context, id string, item plugins.Installation) consoleapi.PluginInstallationView {
	view := consoleapi.PluginInstallationView{ID: id, Installation: item, Targets: []consoleapi.PluginTargetView{}}
	names := make([]string, 0, len(item.Targets))
	for node := range item.Targets {
		names = append(names, node)
	}
	slices.Sort(names)
	for _, node := range names {
		target := consoleapi.PluginTargetView{Node: node, State: "pending"}
		deployment, err := item.Deployment(id, node)
		switch {
		case err != nil:
			target.State = "unavailable"
			target.Error = err.Error()
		case s.coordinator(node) != nil:
			target.State = "unavailable"
			target.Error = ErrNoCoordinator.Error()
		default:
			hash, err := deployment.Hash()
			if err != nil {
				target.Error = err.Error()
			} else {
				if receipt, err := s.Library.Deployment(ctx, hash); err == nil {
					target.State = plugins.Prepared
					target.Receipt = &receipt
				}
				s.mu.Lock()
				observed, known := s.observations[hash]
				s.mu.Unlock()
				if known {
					target = observed
				}
			}
		}
		// A node that is merely down will come back; a hub that is not a
		// coordinator will not become one by waiting, so that reason keeps
		// the target's state rather than being overlaid with "offline".
		if node != "" && s.Admin.Nodes != nil && s.coordinator(node) == nil {
			up := false
			for _, status := range s.Admin.Nodes.Statuses() {
				if status.Name == node {
					up = status.Up
				}
			}
			if !up {
				target.State = "offline"
			}
		}
		view.Targets = append(view.Targets, target)
	}
	return view
}

func (s *PluginService) PreviewPlugin(ctx context.Context, source plugins.Source) (consoleapi.PluginPreview, error) {
	bundle, resolved, err := plugins.Resolve(ctx, source)
	if err != nil {
		return consoleapi.PluginPreview{}, err
	}
	return consoleapi.PluginPreview{Manifest: bundle.Manifest, Digest: bundle.Digest, Source: resolved}, nil
}

func (s *PluginService) ImportPlugin(ctx context.Context, req consoleapi.PluginImportRequest) (record plugins.PackageRecord, err error) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if req.CommandID == "" || req.Project == "" || req.Digest == "" {
		return record, plugins.ErrInvalid
	}
	ConfigMu.RLock()
	declared, exists := s.Admin.Cfg.Projects[req.Project]
	level := s.Admin.Cfg.HubLevel()
	ConfigMu.RUnlock()
	if !exists {
		return record, errors.New("plugin import project is unknown")
	}
	if !s.Admin.ClusterMode && !project.Level(declared.Level).OrDefault().Admits(level) {
		return record, errors.New("coordinator cannot hold this project package")
	}
	operation, err := s.Library.BeginOperation(ctx, req.CommandID, "import", req)
	if err != nil {
		return record, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err = errors.Join(err, s.Library.FinishOperation(cleanup, operation, err))
	}()
	if existing, loadErr := s.Library.Record(ctx, req.Project, req.Digest); loadErr == nil {
		return existing, nil
	} else if !errors.Is(loadErr, plugins.ErrUnavailable) {
		return record, loadErr
	}
	bundle, _, err := plugins.Resolve(ctx, req.Source)
	if err != nil {
		return record, err
	}
	if bundle.Digest != req.Digest {
		return record, plugins.ErrIntegrity
	}
	return s.Library.Add(ctx, req.Project, bundle)
}

func (s *PluginService) UpdatePlugin(ctx context.Context, id string, req consoleapi.PluginUpdateRequest) (consoleapi.PluginsView, error) {
	s.Admin.Mu.Lock()
	defer s.Admin.Mu.Unlock()
	if s.RuntimeGate != nil {
		s.RuntimeGate.Lock()
		defer s.RuntimeGate.Unlock()
	}
	ConfigMu.RLock()
	candidate := *s.Admin.Cfg
	candidate.Plugins = config.ClonePluginInstallations(s.Admin.Cfg.Plugins)
	revision := pluginRevision(candidate.Plugins)
	ConfigMu.RUnlock()
	if req.BaseRevision == "" || req.BaseRevision != revision {
		return consoleapi.PluginsView{}, consoleapi.ErrSettingsConflict
	}
	if prior, exists := candidate.Plugins[id]; exists && prior.PackageID != req.Installation.PackageID {
		return consoleapi.PluginsView{}, errors.New("use a new installation name for a different package")
	}
	if candidate.Plugins == nil {
		candidate.Plugins = map[string]plugins.Installation{}
	}
	previous := candidate.Plugins[id]
	candidate.Plugins[id] = req.Installation
	candidate.Plugins = config.ClonePluginInstallations(candidate.Plugins)
	if err := candidate.ValidatePlugins(); err != nil {
		return consoleapi.PluginsView{}, err
	}
	for _, project := range req.Installation.Projects {
		record, err := s.Library.Record(ctx, project, req.Installation.Digest)
		if err != nil {
			return consoleapi.PluginsView{}, err
		}
		if record.Manifest.ID != req.Installation.PackageID {
			return consoleapi.PluginsView{}, plugins.ErrIntegrity
		}
		for _, configuration := range req.Installation.Targets {
			if err := record.Manifest.CheckConfiguration(configuration); err != nil {
				return consoleapi.PluginsView{}, err
			}
		}
	}
	if err := s.Library.RememberTargets(ctx, id, previous); err != nil {
		return consoleapi.PluginsView{}, err
	}
	if err := s.Library.RememberTargets(ctx, id, candidate.Plugins[id]); err != nil {
		return consoleapi.PluginsView{}, err
	}
	ConfigMu.Lock()
	if pluginRevision(s.Admin.Cfg.Plugins) != revision {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, consoleapi.ErrSettingsConflict
	}
	pluginsCandidate := candidate.Plugins
	candidate = *s.Admin.Cfg
	candidate.Plugins = pluginsCandidate
	if err := candidate.ValidatePlugins(); err != nil {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, err
	}
	saveErr := s.Admin.persistConfigContext(ctx, &candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		ConfigMu.Unlock()
		return consoleapi.PluginsView{}, saveErr
	}
	s.Admin.Cfg.Plugins = candidate.Plugins
	ConfigMu.Unlock()
	view, err := s.Plugins(ctx)
	if err != nil {
		return view, err
	}
	if saveErr != nil {
		view.Warning = saveErr.Error()
	}
	return view, nil
}

func (s *PluginService) PreparePlugin(ctx context.Context, id string) (consoleapi.PluginInstallationView, error) {
	_, items := s.snapshot()
	item, exists := items[id]
	if !exists {
		return consoleapi.PluginInstallationView{}, errors.New("plugin installation is unknown")
	}
	names := make([]string, 0, len(item.Targets))
	for node := range item.Targets {
		names = append(names, node)
	}
	slices.Sort(names)
	for _, node := range names {
		if err := ctx.Err(); err != nil {
			return s.installationView(ctx, id, item), err
		}
		deployment, err := item.Deployment(id, node)
		if err != nil {
			return consoleapi.PluginInstallationView{}, err
		}
		hash, err := deployment.Hash()
		if err != nil {
			return consoleapi.PluginInstallationView{}, err
		}
		target := consoleapi.PluginTargetView{Node: node, State: plugins.Prepared}
		receipt, err := s.prepareTarget(ctx, deployment)
		if err != nil {
			target.State = "unavailable"
			target.Error = err.Error()
		} else {
			target.Receipt = &receipt
		}
		s.mu.Lock()
		if s.observations == nil {
			s.observations = map[string]consoleapi.PluginTargetView{}
		}
		s.observations[hash] = target
		s.mu.Unlock()
	}
	return s.installationView(ctx, id, item), nil
}

func (s *PluginService) prepareTarget(ctx context.Context, d plugins.Deployment) (plugins.DeploymentReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bundle, err := s.Library.Get(ctx, d.Projects[0], d.Digest)
	if err != nil {
		return plugins.DeploymentReceipt{}, err
	}
	if err := s.coordinator(d.Node); err != nil {
		return plugins.DeploymentReceipt{}, err
	}
	var receipt plugins.DeploymentReceipt
	if d.Node == "" {
		receipt, err = s.Local.Store.PrepareDeployment(ctx, d, plugins.Environment{})
	} else {
		var reply nodewire.PluginReply
		reply, err = s.Admin.Nodes.Plugins(ctx, d.Node, nodewire.PluginRequest{Action: nodewire.PluginPrepare, Authority: s.Authority, Deployment: d, Bundle: bundle.Data})
		if err == nil {
			receipt = *reply.Receipt
		}
	}
	if err != nil {
		return receipt, err
	}
	return receipt, s.Library.RecordDeployment(ctx, receipt)
}

func (s *PluginService) PluginSecrets(ctx context.Context, node string) ([]plugins.SecretInfo, error) {
	node = s.Admin.nodeKey(node)
	if node == "" {
		return s.Local.Store.Secrets()
	}
	if err := s.coordinator(node); err != nil {
		return nil, err
	}
	reply, err := s.Admin.Nodes.Plugins(ctx, node, nodewire.PluginRequest{Action: nodewire.PluginSecrets, Authority: s.Authority})
	return reply.Secrets, err
}

func (s *PluginService) Reconcile(ctx context.Context) {
	_, items := s.snapshot()
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.PreparePlugin(ctx, id); err != nil {
			return
		}
	}
}

var _ consoleapi.PluginsService = (*PluginService)(nil)

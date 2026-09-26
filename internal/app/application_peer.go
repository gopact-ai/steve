package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/logs"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func OpenClusterPeer(ctx context.Context, options cluster.PeerOptions) (*cluster.Peer, error) {
	logs.Install()
	hub := &hubLanguage{}
	if options.Text.IsZero() {
		// A configuration that does not load is OpenPeer's to report.
		if installed, err := config.Load(options.ConfigPath); err == nil {
			hub.installed = i18n.FromLang(installed.EffectiveLocale())
			options.Text = i18n.Dynamic(hub.locale)
		}
	}
	options.StartApplication = func(ctx context.Context, p cluster.ApplicationHost, activation cluster.Activation, ready func(cluster.PeerApplicationEndpoint) error) (cluster.Deactivate, error) {
		return startPeerApplication(ctx, p, activation, ready, hub)
	}
	options.SSHHandler = func(service cluster.SSHControl, token, origin string) (http.Handler, error) {
		return httpapi.SSHHandler(service, token, origin)
	}
	return cluster.OpenPeer(ctx, options)
}

// hubLanguage is the Hub's language as this node knows it. In a cluster
// the owner sets it in the shared configuration, which the application
// reads live once it runs on this node; until then, and on a node it has
// never run on, it is the language the node's own configuration names. A
// node the application has left keeps the last language it saw.
type hubLanguage struct {
	installed i18n.Locale
	live      atomic.Pointer[config.RuntimeSettings]
}

func (h *hubLanguage) locale() i18n.Locale {
	if settings := h.live.Load(); settings != nil {
		return i18n.FromLang(settings.Load().Gateway.Locale)
	}
	return h.installed
}

func startPeerApplication(ctx context.Context, p cluster.ApplicationHost, activation cluster.Activation, ready func(cluster.PeerApplicationEndpoint) error, hub *hubLanguage) (cluster.Deactivate, error) {
	token := cluster.ClusterRandomToken()
	started := make(chan struct{})
	done := make(chan struct{})
	var runErr error
	stopRepair := func() {}
	var repairObserve cluster.ContentRepairObservation
	stateConfig := newApplicationConfiguration(ctx, activation, p.Worker())
	content, err := p.ContentReplicator(activation)
	if err != nil {
		return nil, err
	}
	environment := &Environment{Ledger: activation.Ledger, NodeID: activation.NodeID, Coordination: p, Configure: func(cfg *config.Config) error {
		cfg.Gateway.HubID = p.ApplicationClusterID()
		if cfg.Nodes == nil {
			cfg.Nodes = map[string]config.Node{}
		}
		cfg.Nodes[p.Worker().Name] = config.Node{Addr: p.Worker().Address, Token: p.Worker().Token}
		if root := p.WorkerWorkspaceRoot(); root != "" {
			cfg.Gateway.WorkspaceRoot = root
		}
		if err := stateConfig.Configure(cfg); err != nil {
			return err
		}
		return p.ConfigureApplication(cfg, activation)
	}, Ready: func(admin *adminsvc.Service, dashboard *httpapi.Server) error {
		if admin.RuntimeSettings != nil {
			hub.live.Store(admin.RuntimeSettings)
		}
		if admin.View != nil {
			repairObserve = admin.View.Observe
		}
		if err := p.ApplicationReady(admin, dashboard, activation); err != nil {
			return err
		}
		if err := ready(cluster.PeerApplicationEndpoint{URL: dashboard.URL(), Token: token, Admin: admin}); err != nil {
			return err
		}
		close(started)
		return nil
	}}
	environment.HTTPConfig = &httpapi.ServerConfig{Addr: "127.0.0.1:0", Token: token}
	environment.ConfigureNodes = p.ConfigureNodes
	environment.WriteConfig = stateConfig.Save
	environment.WriteConfigContext = stateConfig.SaveContext
	environment.ConfigurationRevision = stateConfig.Revision
	environment.SessionBinder = newApplicationSessionBinder(activation)
	environment.SessionAuthorizer = p.ApplicationSessionAuthorizer(activation)
	environment.ReceiptAuthorizer = p.ApplicationReceiptAuthorizer(activation)
	environment.PluginAuthorizer = p.ApplicationPluginAuthorizer(activation)
	environment.PluginAuthority = nodewire.SessionAuthority{ClusterID: p.ApplicationClusterID(), CoordinatorNodeID: activation.NodeID, CoordinatorEpoch: activation.Assignment.Epoch, WriterGeneration: activation.WriterGeneration}
	environment.Content = content
	environment.Fail = func(err error) { p.ApplicationStoreFailure(activation, err) }
	go func() {
		application, err := Build(ctx, Config{Path: p.ApplicationConfigPath(), Environment: environment})
		if err != nil {
			runErr = err
		} else {
			runErr = application.Run(ctx)
		}
		if ctx.Err() != nil && cluster.ApplicationAuthorityError(runErr) {
			runErr = ctx.Err()
		}
		var restart *adminsvc.RestartExit
		expectedRestart := errors.As(runErr, &restart)
		replaceProgram := expectedRestart && restart.Program != ""
		if replaceProgram && ctx.Err() == nil {
			// Becoming another build is not something this activation can
			// do to itself: the runtime stops with the restart as its
			// cause, and the process continues as the named program.
			activation.Runtime.FailGeneration(activation.Generation, runErr)
		} else if expectedRestart && ctx.Err() == nil {
			runErr = activation.Runtime.RestartGeneration(activation.Generation)
		} else if expectedRestart {
			runErr = nil
		}
		if runErr == nil && ctx.Err() == nil && !expectedRestart {
			runErr = errors.New("business application exited unexpectedly")
		}
		close(done)
		if ctx.Err() == nil && !expectedRestart {
			if cluster.ApplicationAuthorityError(runErr) {
				// RequestRebuild only refuses (ErrInactive) when this
				// generation has already ended, and then there is nothing
				// left to rebuild; the cause is logged by the runtime.
				_ = activation.Runtime.RequestRebuild(activation.Generation, runErr)
			} else {
				activation.Runtime.FailGeneration(activation.Generation, runErr)
			}
		}
	}()
	stop := func(context.Context) error {
		started := time.Now()
		stopRepair()
		if took := time.Since(started); took >= defaultSlowStep {
			slog.Warn("app: content repair stop is slow", "generation", activation.Generation, "took", took.Round(time.Millisecond))
		}
		<-done
		var restart *adminsvc.RestartExit
		if errors.Is(runErr, context.Canceled) || errors.As(runErr, &restart) || ctx.Err() != nil && cluster.ApplicationAuthorityError(runErr) {
			return nil
		}
		return runErr
	}
	select {
	case <-started:
		if ctx.Err() == nil {
			stopRepair = p.StartContentRepair(activation, repairObserve)
		}
		return stop, nil
	case <-done:
		return stop, runErr
	case <-ctx.Done():
		return stop, ctx.Err()
	}
}

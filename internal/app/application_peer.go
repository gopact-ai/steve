package app

import (
	"context"
	"errors"
	"net/http"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/httpapi"
)

func OpenClusterPeer(ctx context.Context, options cluster.PeerOptions) (*cluster.Peer, error) {
	configureLogging()
	options.StartApplication = startPeerApplication
	options.SSHHandler = func(service cluster.SSHControl, token, origin string) (http.Handler, error) {
		return httpapi.SSHHandler(service, token, origin)
	}
	return cluster.OpenPeer(ctx, options)
}
func startPeerApplication(ctx context.Context, p cluster.ApplicationHost, activation cluster.Activation, ready func(cluster.PeerApplicationEndpoint) error) (cluster.Deactivate, error) {
	token, err := cluster.ClusterRandomToken()
	if err != nil {
		return nil, err
	}
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
		cfg.Harnesses = map[string]config.Harness{}
		if cfg.Nodes == nil {
			cfg.Nodes = map[string]config.Node{}
		}
		cfg.Nodes[p.Worker().Name] = config.Node{Addr: p.Worker().Address, Token: p.Worker().Token}
		if err := stateConfig.Configure(cfg); err != nil {
			return err
		}
		return p.ConfigureApplication(cfg, activation)
	}, Ready: func(admin *adminsvc.Service, dashboard *httpapi.Server) error {
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
		if expectedRestart && ctx.Err() == nil {
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
				_ = activation.Runtime.RequestRebuild(activation.Generation, runErr)
			} else {
				activation.Runtime.FailGeneration(activation.Generation, runErr)
			}
		}
	}()
	stop := func(context.Context) error {
		stopRepair()
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

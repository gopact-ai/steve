package cluster

import (
	"context"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// ApplicationHost is the peer's application-facing port. The application
// runner uses these capabilities without depending on the peer implementation.
type ApplicationHost interface {
	consoleapi.CoordinationService
	Worker() PeerWorkerDescriptor
	ApplicationConfigPath() string
	ApplicationClusterID() string
	ConfigureApplication(*config.Config, Activation) error
	ApplicationReady(*adminsvc.Service, ApplicationServer, Activation) error
	ConfigureNodes(map[string]node.Config) error
	ContentReplicator(Activation) (contentreplica.Replicator, error)
	ApplicationSessionAuthorizer(Activation) func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error
	ApplicationStoreFailure(Activation, error)
	StartContentRepair(Activation, ContentRepairObservation) func()
}

func (p *Peer) ApplicationConfigPath() string { return p.Options.ConfigPath }
func (p *Peer) ApplicationClusterID() string  { return p.Config.ClusterID }
func (p *Peer) ConfigureApplication(cfg *config.Config, activation Activation) error {
	if p.Options.ConfigureApplication != nil {
		return p.Options.ConfigureApplication(cfg, activation)
	}
	return nil
}
func (p *Peer) ApplicationReady(admin *adminsvc.Service, dashboard ApplicationServer, activation Activation) error {
	if p.Options.ApplicationReady != nil {
		return p.Options.ApplicationReady(admin, dashboard, activation)
	}
	return nil
}

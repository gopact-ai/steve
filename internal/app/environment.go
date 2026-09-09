package app

import (
	"context"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type Environment struct {
	PluginAuthority       nodewire.SessionAuthority
	PluginAuthorizer      func(context.Context, string, nodewire.PluginRequest) error
	Ledger                *ledger.Ledger
	Content               contentreplica.Replicator
	NodeID                string
	HTTPConfig            *httpapi.ServerConfig
	WriteConfig           func(string, *config.Config) error
	WriteConfigContext    func(context.Context, string, *config.Config) error
	ConfigureNodes        func(map[string]node.Config) error
	SessionBinder         func(context.Context, harness.Placement, string, string) (context.Context, error)
	SessionAuthorizer     func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error
	Fail                  func(error)
	ConfigurationRevision func() string
	Configure             func(*config.Config) error
	Coordination          consoleapi.CoordinationService
	Ready                 func(*adminsvc.Service, *httpapi.Server) error
}

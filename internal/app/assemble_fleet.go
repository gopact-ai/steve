package app

import (
	"fmt"
	"log/slog"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

func assembleFleet(life lifetime, input inputAssembly, boot runtimeAssembly) (fleetAssembly, error) {
	environment := input.Environment()
	book := boot.Book()
	catalog := boot.Catalog()
	cfg := boot.Config()
	ctx := boot.Context()
	manager := boot.Manager()
	nodewire.SetSelf(adminsvc.NodeName())
	nodeConfigs := cfg.NodeConfigs()
	if environment != nil && environment.ConfigureNodes != nil {
		if err := environment.ConfigureNodes(nodeConfigs); err != nil {
			return nil, err
		}
	}
	nodes := node.NewRegistry(cfg.Gateway.HubID, nodeConfigs)
	if environment != nil {
		nodes.SetSessionAuthorizer(environment.SessionAuthorizer)
		nodes.SetPluginAuthorizer(environment.PluginAuthorizer)
	}
	life.Defer(func() { nodes.Close() })
	manager.SetTransports(nodes)

	// Projects are the hub's assignment, recorded in the ledger from config
	// at every boot. Steve's own home directory is a project too — the one
	// the owner's DM works in — so nothing special-cases it downstream.
	projects := project.Open(book, artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	projects.SetHubID(cfg.Gateway.HubID)
	if err := (config.ProjectController{Store: projects}).Reconcile(ctx, cfg); err != nil {
		return nil, fmt.Errorf("reconcile configured projects: %w", err)
	}
	for _, note := range cfg.Migrated {
		slog.Info(fmt.Sprintf("steve: config migrated: %s", note))
	}

	// The roster is what turns "which agents exist" into "which agents can
	// run this right now", from live adverts rather than from config.
	fleet := roster.New(catalog)
	fleet.SetNodes(nodes)
	fleet.SetHubCapabilities(cfg.Gateway.Capabilities)
	observation, closeObservation := newLocalObservation(ctx, cfg)
	life.Defer(func() { closeObservation() })
	fleet.SetHubAdvert(func() nodewire.Advert { return adminsvc.ObservedHubAdvert(cfg, observation) })
	fleet.SetHubLevel(cfg.HubLevel())
	return &fleetValues{fleet: fleet, nodes: nodes, observation: observation, projects: projects}, nil
}

type fleetAssembly interface {
	Fleet() *roster.Roster
	Nodes() *node.Registry
	Observation() *adminsvc.LocalObservation
	Projects() *project.Store
}

type fleetValues struct {
	fleet       *roster.Roster
	nodes       *node.Registry
	observation *adminsvc.LocalObservation
	projects    *project.Store
}

func (v *fleetValues) Fleet() *roster.Roster { return v.fleet }

func (v *fleetValues) Nodes() *node.Registry { return v.nodes }

func (v *fleetValues) Observation() *adminsvc.LocalObservation { return v.observation }

func (v *fleetValues) Projects() *project.Store { return v.projects }

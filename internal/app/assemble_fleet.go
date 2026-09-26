package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/configbuild"
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
	nodewire.SetSelf(boot.NodeName())
	// Machines are stored by identity and read by name. Everything said to
	// a person resolves the one into the other from here on; a machine
	// nobody has named still reads as its identity. The names are kept in
	// a snapshot refreshed on its own: rendering a sentence must never
	// reach into the consensus replica, which is applying the very write
	// whose failure is being described.
	if environment != nil && environment.Coordination != nil {
		nodewire.SetNames(publishedNames(boot.Background(), environment.Coordination.MemberNames))
	}
	nodeConfigs := configbuild.NodeConfigs(cfg)
	if environment != nil && environment.ConfigureNodes != nil {
		if err := environment.ConfigureNodes(nodeConfigs); err != nil {
			return nil, err
		}
	}
	nodes := node.NewRegistry(cfg.Gateway.HubID, nodeConfigs)
	if environment != nil {
		nodes.SetSessionAuthorizer(environment.SessionAuthorizer)
		nodes.SetNodeReceiptAuthorizer(environment.ReceiptAuthorizer)
		nodes.SetPluginAuthorizer(environment.PluginAuthorizer)
	}
	life.Defer(func() { nodes.Close() })
	manager.SetTransports(nodes)

	// Projects are the hub's assignment, recorded in the ledger from config
	// at every boot. Steve's own home directory is a project too — the one
	// the owner's DM works in — so nothing special-cases it downstream.
	projects := project.Open(book, artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	projects.SetHubID(cfg.Gateway.HubID)
	if err := (configbuild.ProjectController{Store: projects}).Reconcile(hubContext(ctx, cfg), cfg); err != nil {
		return nil, fmt.Errorf("reconcile configured projects: %w", err)
	}

	// The roster is what turns "which agents exist" into "which agents can
	// run this right now", from live adverts rather than from config.
	fleet := roster.New(catalog)
	fleet.SetNodes(nodes)
	fleet.SetHubCapabilities(cfg.Gateway.Capabilities)
	observation, closeObservation := newLocalObservation(ctx, boot.ConfigStore())
	life.Defer(func() { closeObservation() })
	fleet.SetHubAdvert(func() nodewire.Advert {
		return adminsvc.ObservedHubAdvert(boot.NodeName(), boot.ConfigStore(), observation)
	})
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

// publishedNames keeps the machine names people gave in a snapshot that
// readers can take without waiting for anything. A rename shows up within
// a few seconds, which is as fast as anyone reads a sentence about it.
func publishedNames(background *applicationBackground, read func() map[string]string) func() map[string]string {
	var published atomic.Pointer[map[string]string]
	refresh := func() {
		names := read()
		published.Store(&names)
	}
	refresh()
	background.Go(func(ctx context.Context) {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	})
	return func() map[string]string {
		if names := published.Load(); names != nil {
			return *names
		}
		return nil
	}
}

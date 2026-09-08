package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
)

func Doctor(configPath string, timeout time.Duration) error {
	cfg, catalog, manager, live, err := load(configPath)
	if err != nil {
		return err
	}
	defer manager.Stop()
	book, err := openLedger(cfg)
	if err != nil {
		return err
	}
	defer book.Close()
	store, err := state.OpenLedger(book, cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	if err := store.Check(); err != nil {
		return err
	}
	assembler, err := wireHome(cfg, live)
	if err != nil {
		return err
	}
	warnHome(cfg)
	if err := checkHome(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if cfg.FeishuEnabled() {
		identity, err := feishu.Probe(ctx, cfg.Feishu.AppID, cfg.Feishu.AppSecret, cfg.Feishu.Domain)
		if err != nil {
			return err
		}
		log.Printf("steve: feishu bot %s %s", identity.Name, identity.OpenID)
	}

	// Nodes are probed before agents: availability is a real dial, not a
	// line in the config, and an agent placed on an unreachable node should
	// fail with that fact rather than with a mystery session error.
	nodewire.SetSelf(adminsvc.NodeName())
	nodes := node.NewRegistry(cfg.Gateway.HubID, cfg.NodeConfigs())
	defer nodes.Close()
	manager.SetTransports(nodes)

	// Projects are the hub's assignment, recorded in the ledger from config
	// at every boot. Steve's own home directory is a project too — the one
	// the owner's DM works in — so nothing special-cases it downstream.
	projects := project.Open(book, artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	projects.SetHubID(cfg.Gateway.HubID)
	declared, _, err := config.ProjectDeclarations(cfg)
	if err != nil {
		return err
	}
	if err := (config.ProjectController{Store: projects}).Reconcile(ctx, cfg); err != nil {
		return fmt.Errorf("reconcile configured projects: %w", err)
	}
	for _, note := range cfg.Migrated {
		log.Printf("steve: config migrated: %s", note)
	}
	for _, g := range cfg.GrantList() {
		if _, err := projects.Grant(context.Background(), g.Project, g.Principal, g.Role, g.By); err != nil {
			return fmt.Errorf("grant %s in %s: %w", g.Principal, g.Project, err)
		}
	}
	observation, closeObservation := newLocalObservation(ctx, cfg)
	defer closeObservation()
	self := adminsvc.ObservedHubAdvert(cfg, observation)
	log.Printf("steve: hub %s — %s %v, %s/%s, level=%s, harnesses=%s, caps=%v",
		self.Node, self.Hostname, self.IPs, self.OS, self.Arch, cfg.HubLevel(), adminsvc.HarnessSummary(self), self.Capabilities)
	reportGit("hub "+self.Node, self)
	for _, h := range self.Harnesses {
		if h.Missing != "" {
			log.Printf("steve: hub cannot run %s: %s", h.ID, h.Missing)
		}
	}
	for _, status := range nodes.Probe(ctx) {
		if !status.Up {
			return fmt.Errorf("node %q at %s unreachable: %s", status.Name, status.Addr, status.LastError)
		}
		log.Printf("steve: node %s up — %s/%s, level=%s, harnesses=%s, caps=%v",
			status.Name, status.Advert.OS, status.Advert.Arch, status.Level,
			adminsvc.HarnessSummary(status.Advert), status.Advert.Capabilities)
		reportGit("node "+status.Name, status.Advert)
		for _, h := range status.Advert.Harnesses {
			if h.Missing != "" {
				log.Printf("steve: node %s cannot run %s: %s", status.Name, h.ID, h.Missing)
			}
		}
	}

	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.EffectiveOwnerID() != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
		capabilities, err := assembler.AssembleMode(selected, home.ModeGuest)
		if err != nil {
			return fmt.Errorf("agent %q capabilities: %w", selected.ID, err)
		}
		at := harness.Placement{Node: selected.Node, Harness: selected.Harness}
		// An agent is probed in a project that lives where it runs. An agent
		// on a node no project is homed on has nowhere to open a session
		// yet, and that is reported rather than papered over.
		workspace, ok := probeWorkspace(declared, selected.Node)
		if !ok {
			log.Printf("steve: agent %s on %s: no project is homed there; session not probed", selected.ID, at)
			continue
		}
		session, err := manager.OpenSession(ctx, at, "", workspace, capabilities.MCPServers)
		if err != nil {
			return fmt.Errorf("agent %q session on %s: %w", selected.ID, at, err)
		}
		if err := manager.CloseSession(ctx, at, session.ID()); err != nil {
			return fmt.Errorf("agent %q close session: %w", selected.ID, err)
		}
	}
	if names := live.Map.EnabledNames(); len(names) > 0 {
		log.Printf("steve: skills %s", strings.Join(names, ","))
	} else {
		log.Printf("steve: skills none")
	}
	log.Printf("steve: doctor passed")
	return nil
}

// probeWorkspace picks a project directory on node for a doctor probe: the
// default project's if it is homed there, else any project's.
func probeWorkspace(projects []project.Project, node string) (string, bool) {
	for _, p := range projects {
		if p.ID == adminsvc.HomeProjectID {
			continue
		}
		if ws, err := p.Place(node); err == nil {
			return ws.Path, true
		}
	}
	for _, p := range projects {
		if ws, err := p.Place(node); err == nil {
			return ws.Path, true
		}
	}
	return "", false
}

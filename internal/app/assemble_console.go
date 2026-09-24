package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/localtoken"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/sameorigin"
)

func assembleConsole(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly, work executionAssembly, planning plansAssembly, projection readModelAssembly) (consoleAssembly, error) {
	// The administration service names this machine by it; an empty name
	// would be taken for no machine at all.
	if boot.NodeName() == "" {
		return nil, errors.New("console: this machine has no node name")
	}
	configPath := input.ConfigPath()
	environment := input.Environment()
	book := boot.Book()
	catalog := boot.Catalog()
	cfg := boot.Config()
	ctx := boot.Context()
	live := boot.Live()
	manager := boot.Manager()
	stop := boot.Stop()
	attempts := storage.Attempts()
	assembler := identity.Assembler()
	profile := identity.Profile()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	observation := machines.Observation()
	projects := machines.Projects()
	artifacts := work.Artifacts()
	coordinator := work.Coordinator()
	executions := work.Executions()
	memories := work.Memories()
	tasks := work.Tasks()
	auxiliary := planning.Auxiliary()
	stepRunner := planning.StepRunner()
	repos := projection.Repos()
	shipper := projection.Shipper()
	view := projection.View()
	httpConfig, err := consoleServerConfig(environment, cfg)
	if err != nil {
		return nil, err
	}
	dashboard, err := httpapi.NewServer(view, httpConfig)
	if err != nil {
		return nil, err
	}
	life.Defer(func() { dashboard.Close() })
	if environment == nil {
		if err := pinDesktopAddress(boot.ConfigStore(), *configPath, dashboard.URL()); err != nil {
			return nil, err
		}
	}
	// The console: the owner acting from the page, through this same
	// coordinator. Notices anchored on the console stay on the page.
	cons := console.New(coordinator, cfg.EffectiveOwnerID(), view)
	coordinator.SetConsoleCompletionGuard(console.CheckTaskCompletionTx)
	cons.SetRecoveryQuiet(time.Duration(cfg.Gateway.RecoveryQuiet))
	if environment != nil {
		cons.EnableRetainedRecovery(ctx)
	}
	ask, askUser := planQuestions(cons, tasks, attempts)
	stepRunner.SetQuestionHandlers(ask, askUser)
	auxiliary.SetQuestionHandlers(ask, askUser)
	view.SetInteractions(cons)
	cons.SetTitler(&conversationTitler{manager: manager, catalog: catalog, projects: projects, home: cfg.Gateway.HomePath})
	dashboard.SetConsole(cons)
	dashboard.SetChannelHistory(&channelConversations{
		ChannelHistory: gateway.NewChannelHistory(book), contexts: coordinator, tasks: tasks, activity: work.Gateway(),
	})
	// A copy may only sit where the project's level admits; the store
	// asks the registry, which knows every machine's level.
	projects.Levels = func(node string) datalevel.Level {
		level, err := nodes.Level(context.Background(), node)
		if err != nil {
			return ""
		}
		return datalevel.Level(level)
	}
	admin := &adminsvc.Service{Lifetime: ctx, NodeName: boot.NodeName(), ConfigStore: boot.ConfigStore(), Path: *configPath, Nodes: nodes, Catalog: catalog, Fleet: fleet, Manager: manager, Assembler: assembler, Projects: projects, Repos: repos, Attempts: attempts, Tasks: tasks, View: view,
		LiveSkills: live, Shipper: shipper, Observation: observation, Coordinator: coordinator, HomePath: cfg.Gateway.HomePath, Memory: memories, Artifacts: artifacts}
	admin.RuntimeSettings = boot.Settings()
	life.Defer(func() { admin.CloseSSH() })
	if environment != nil {
		admin.WriteConfig = environment.WriteConfig
		admin.WriteConfigContext = environment.WriteConfigContext
		admin.ConfigRevision = environment.ConfigurationRevision
		admin.ClusterMode = true
		admin.Members = environment.Coordination
	}
	admin.HomeLoader, admin.SharedHome = profile.Home, profile.Shared
	dashboard.SetAdmin(admin)
	dashboard.SetDesktop(admin)
	dashboard.SetSSH(admin)
	if environment != nil {
		dashboard.SetCoordination(environment.Coordination)
	}
	cons.SetInspector(admin)
	materials, err := material.Open(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "materials"), book)
	if err != nil {
		return nil, fmt.Errorf("open materials: %w", err)
	}
	life.Defer(func() { materials.Close() })
	reconciliations := &reconciliationWorkers{}
	life.Defer(func() { stop(); reconciliations.Close() })
	if environment != nil {
		materials.SetReplication(environment.Content)
	}
	life.Defer(func() {
		stop()
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpDone := make(chan error, 1)
		go func() { httpDone <- dashboard.Shutdown(cleanup) }()
		if err := cons.Shutdown(cleanup); err != nil {
			slog.Error(fmt.Sprintf("steve: console shutdown incomplete: %v", err))
			*life.RunError() = errors.Join(*life.RunError(), err)
		}
		if err := executions.Shutdown(cleanup); err != nil {
			slog.Error(fmt.Sprintf("steve: execution shutdown incomplete; startup will reconcile remaining writers: %v", err))
			*life.RunError() = errors.Join(*life.RunError(), err)
		}
		if err := <-httpDone; err != nil {
			slog.Error(fmt.Sprintf("steve: HTTP shutdown incomplete: %v", err))
			*life.RunError() = errors.Join(*life.RunError(), err)
		}
		manager.Stop()
	})
	return &consoleValues{admin: admin, cons: cons, dashboard: dashboard, materials: materials, reconciliations: reconciliations}, nil
}

type consoleAssembly interface {
	Admin() *adminsvc.Service
	Console() *console.Service
	Dashboard() *httpapi.Server
	Materials() *material.Store
	Reconciliations() *reconciliationWorkers
}

type consoleValues struct {
	admin           *adminsvc.Service
	cons            *console.Service
	dashboard       *httpapi.Server
	materials       *material.Store
	reconciliations *reconciliationWorkers
}

func (v *consoleValues) Admin() *adminsvc.Service { return v.admin }

func (v *consoleValues) Console() *console.Service { return v.cons }

func (v *consoleValues) Dashboard() *httpapi.Server { return v.dashboard }

func (v *consoleValues) Materials() *material.Store { return v.materials }

func (v *consoleValues) Reconciliations() *reconciliationWorkers { return v.reconciliations }

// consoleServerConfig takes the listener a cluster member was handed as is.
// Otherwise it never serves the console without a token. Loopback keeps
// other machines out, not other users and processes on this one. A generated
// token stays out of the configuration: restarts compare the configured token
// with the one the process booted with, and it is not the owner's setting.
func consoleServerConfig(environment *Environment, cfg *config.Config) (httpapi.ServerConfig, error) {
	if environment != nil && environment.HTTPConfig != nil {
		return *environment.HTTPConfig, nil
	}
	served := httpapi.ServerConfig{Addr: cfg.Gateway.ReadModelAddr, Token: cfg.Gateway.ReadModelToken}
	if served.Token != "" {
		return served, nil
	}
	// Serving the network is the owner's decision, token included.
	if !sameorigin.LoopbackListener(served.Addr) {
		return httpapi.ServerConfig{}, fmt.Errorf("gateway.read_model_addr %s is not a loopback address: set gateway.read_model_token to serve the console there", served.Addr)
	}
	token, err := localtoken.Resolve(filepath.Dir(cfg.Gateway.StatePath))
	if err != nil {
		return httpapi.ServerConfig{}, fmt.Errorf("console token: %w", err)
	}
	served.Token = token
	return served, nil
}

// pinDesktopAddress records the console's address in the configuration. The
// launch probe already reads the configuration, so the pinned address is
// saved and published through its store; nothing is written when the address
// is unchanged.
func pinDesktopAddress(store *adminsvc.ConfigStore, path, url string) error {
	pinned := false
	err := store.Update(func(c *config.Config) (err error) {
		pinned, err = desktop.SetPinnedAddress(path, c, url)
		return err
	}, func(c *config.Config) error {
		if !pinned {
			return nil
		}
		return config.Save(path, c)
	})
	if err != nil {
		return fmt.Errorf("remember desktop address: %w", err)
	}
	return nil
}

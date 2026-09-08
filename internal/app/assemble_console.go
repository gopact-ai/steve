package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
)

func assembleConsole(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly, work executionAssembly, planning plansAssembly, projection readModelAssembly) (consoleAssembly, error) {
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
	httpConfig := httpapi.ServerConfig{
		Addr: cfg.Gateway.ReadModelAddr, Token: cfg.Gateway.ReadModelToken,
	}
	if environment != nil && environment.HTTPConfig != nil {
		httpConfig = *environment.HTTPConfig
	}
	dashboard, err := httpapi.NewServer(view, httpConfig)
	if err != nil {
		return nil, err
	}
	life.Defer(func() { dashboard.Close() })
	if environment == nil {
		if err := desktop.PinAddress(*configPath, cfg, dashboard.URL()); err != nil {
			return nil, fmt.Errorf("remember desktop address: %w", err)
		}
	}
	// The console: the owner acting from the page, through this same
	// coordinator. Notices anchored on the console stay on the page.
	cons := console.New(coordinator, cfg.EffectiveOwnerID(), view)
	if environment != nil {
		cons.EnableRetainedRecovery(ctx)
		ask, askUser := nativePlanQuestions(cons, tasks, attempts)
		stepRunner.SetQuestionHandlers(ask, askUser)
		auxiliary.SetQuestionHandlers(ask, askUser)
	}
	view.SetInteractions(cons)
	cons.SetTitler(&conversationTitler{manager: manager, catalog: catalog, projects: projects, home: cfg.Gateway.HomePath})
	dashboard.SetConsole(cons)
	// A copy may only sit where the project's level admits; the store
	// asks the registry, which knows every machine's level.
	projects.Levels = func(node string) project.Level {
		level, err := nodes.Level(context.Background(), node)
		if err != nil {
			return ""
		}
		return project.Level(level)
	}
	admin := &adminsvc.Service{Lifetime: ctx, Cfg: cfg, Path: *configPath, Nodes: nodes, Catalog: catalog, Fleet: fleet, Manager: manager, Assembler: assembler, Projects: projects, Repos: repos, Attempts: attempts, Tasks: tasks, View: view,
		LiveSkills: live, Shipper: shipper, Observation: observation, Coordinator: coordinator, HomePath: cfg.Gateway.HomePath, Memory: memories, Artifacts: artifacts}
	life.Defer(func() { admin.CloseSSH() })
	if environment != nil {
		admin.WriteConfig = environment.WriteConfig
		admin.WriteConfigContext = environment.WriteConfigContext
		admin.ConfigRevision = environment.ConfigurationRevision
		admin.ClusterMode = true
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
	reconciliations := &sync.WaitGroup{}
	life.Defer(func() { stop(); reconciliations.Wait() })
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
	Reconciliations() *sync.WaitGroup
}

type consoleValues struct {
	admin           *adminsvc.Service
	cons            *console.Service
	dashboard       *httpapi.Server
	materials       *material.Store
	reconciliations *sync.WaitGroup
}

func (v *consoleValues) Admin() *adminsvc.Service { return v.admin }

func (v *consoleValues) Console() *console.Service { return v.cons }

func (v *consoleValues) Dashboard() *httpapi.Server { return v.dashboard }

func (v *consoleValues) Materials() *material.Store { return v.materials }

func (v *consoleValues) Reconciliations() *sync.WaitGroup { return v.reconciliations }

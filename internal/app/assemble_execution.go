package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

func assembleExecution(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly) (executionAssembly, error) {
	environment := input.Environment()
	background := boot.Background()
	book := boot.Book()
	catalog := boot.Catalog()
	cfg := boot.Config()
	ctx := boot.Context()
	live := boot.Live()
	manager := boot.Manager()
	attempts := storage.Attempts()
	store := storage.Store()
	assembler := identity.Assembler()
	profile := identity.Profile()
	nodes := machines.Nodes()
	projects := machines.Projects()

	coordinator := turn.New(
		catalog, store, assembler, manager, time.Duration(cfg.Gateway.PromptTimeout),
	)
	catalogText := i18n.New(i18n.FromLang(cfg.EffectiveLocale()))
	coordinator.SetIdentity(cfg.EffectiveOwnerID(), profile.Home)
	if cfg.FeishuEnabled() {
		if err := coordinator.SetChannelOwner("feishu", cfg.Feishu.OwnerOpenID); err != nil {
			return nil, err
		}
	}
	coordinator.SetSkills(live)
	coordinator.SetProjects(projects, cfg.Gateway.DefaultProject, adminsvc.HomeProjectID)
	// Memory: the home's MEMORY.md for the owner, one file per project,
	// every write locked and audited under the state directory.
	memories := profile.Service
	coordinator.SetMemory(memories)
	// Recovery classification already ran before project reconciliation;
	// uncertain writers retain their physical directory ownership.
	background.Go(func(ctx context.Context) { sweepAttempts(ctx, attempts) })
	coordinator.SetAttempts(attempts)
	// Artifacts: every result is a commit in the project's shadow
	// repository on the hub, materialised wherever a step runs.
	artifacts := artifact.New(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "artifacts"), book, projects, nodes)
	if environment != nil {
		artifacts.SetReplication(environment.Content)
	}
	artifacts.Direct = cfg.Gateway.DirectTransfer
	artifacts.Limits = artifact.Limits{MaxFiles: cfg.Policies.Snapshot.MaxFiles, MaxBytes: cfg.Policies.Snapshot.MaxBytes, MaxFileBytes: cfg.Policies.Snapshot.MaxFileBytes}
	artifacts.Review = artifact.ReviewLimits{MaxChanges: cfg.Policies.Review.MaxChanges, MaxDiffBytes: cfg.Policies.Review.MaxDiffBytes, MaxFileBytes: cfg.Policies.Review.MaxFileBytes, MaxEntries: cfg.Policies.Review.MaxEntries, Timeout: time.Duration(cfg.Policies.Review.Timeout)}
	coordinator.SetArtifacts(artifacts)
	// Side effects agents ask for are intents: claimed, journaled, and
	// blocked across attempts until a person resolves an unknown outcome.
	intents := intent.New(book)
	coordinator.SetIntents(intents)
	// A landing the previous process was cut off in is finished — or
	// stopped at a conflict — before any turn can touch the canonical.
	if recovered, err := artifacts.RecoverLandings(ctx); err != nil {
		return nil, fmt.Errorf("recover landings: %w", err)
	} else {
		for _, l := range recovered {
			slog.Info(fmt.Sprintf("steve: recovered landing %s of %s into %s: %s", l.ID, l.Artifact, l.Project, l.State), "landing", l.ID, "artifact", l.Artifact, "project", l.Project)
		}
	}
	tasks, err := task.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "tasks.json"))
	if err != nil {
		return nil, fmt.Errorf("open tasks: %w", err)
	}
	executions := execution.New(ctx, tasks)
	coordinator.SetExecution(executions)
	artifacts.SetExecution(executions)
	tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	coordinator.SetTasks(tasks, adminsvc.NodeName())
	schedules, err := schedule.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "schedules.json"))
	if err != nil {
		return nil, fmt.Errorf("open schedules: %w", err)
	}
	plans, err := plan.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "plans.json"))
	if err != nil {
		return nil, fmt.Errorf("open plans: %w", err)
	}
	coordinator.SetSchedules(schedules)
	coordinator.SetCatalog(catalogText)
	if names := live.Map.EnabledNames(); len(names) > 0 {
		slog.Info(fmt.Sprintf("steve: isolated runtimes; skills=%s", strings.Join(names, ",")))
	} else {
		slog.Info("steve: isolated runtimes; skills=none")
	}
	{
		slog.Info(fmt.Sprintf("steve: serving at most %d conversations at once", gateway.PoolSize()))
	}
	gw := gateway.New(coordinator)
	gw.SetCatalog(catalogText)
	return &executionValues{artifacts: artifacts, catalogText: catalogText, coordinator: coordinator, executions: executions, gw: gw, intents: intents, memories: memories, plans: plans, schedules: schedules, tasks: tasks}, nil
}

type executionAssembly interface {
	Artifacts() *artifact.Store
	CatalogText() i18n.Catalog
	Coordinator() *turn.Coordinator
	Executions() *execution.Registry
	Gateway() *gateway.Gateway
	Intents() *intent.Service
	Memories() *memory.Service
	Plans() *plan.Store
	Schedules() *schedule.Store
	Tasks() *task.Store
}

type executionValues struct {
	artifacts   *artifact.Store
	catalogText i18n.Catalog
	coordinator *turn.Coordinator
	executions  *execution.Registry
	gw          *gateway.Gateway
	intents     *intent.Service
	memories    *memory.Service
	plans       *plan.Store
	schedules   *schedule.Store
	tasks       *task.Store
}

func (v *executionValues) Artifacts() *artifact.Store { return v.artifacts }

func (v *executionValues) CatalogText() i18n.Catalog { return v.catalogText }

func (v *executionValues) Coordinator() *turn.Coordinator { return v.coordinator }

func (v *executionValues) Executions() *execution.Registry { return v.executions }

func (v *executionValues) Gateway() *gateway.Gateway { return v.gw }

func (v *executionValues) Intents() *intent.Service { return v.intents }

func (v *executionValues) Memories() *memory.Service { return v.memories }

func (v *executionValues) Plans() *plan.Store { return v.plans }

func (v *executionValues) Schedules() *schedule.Store { return v.schedules }

func (v *executionValues) Tasks() *task.Store { return v.tasks }

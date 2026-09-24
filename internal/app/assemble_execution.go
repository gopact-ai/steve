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
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
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

	catalogText := i18n.New(i18n.FromLang(cfg.EffectiveLocale()))
	if settings := boot.Settings(); settings != nil {
		catalogText = i18n.Dynamic(func() i18n.Locale { return i18n.FromLang(settings.Load().Gateway.Locale) })
	}
	// Memory: the home's MEMORY.md for the owner, one file per project,
	// every write locked and audited under the state directory.
	memories := profile.Service
	// Recovery classification already ran before project reconciliation;
	// uncertain writers retain their physical directory ownership.
	background.Go(func(ctx context.Context) { sweepAttempts(ctx, attempts) })
	// Artifacts: every result is a commit in the project's shadow
	// repository on the hub, materialised wherever a step runs.
	artifacts := artifact.New(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "artifacts"), book, projects, nodes)
	if environment != nil {
		artifacts.SetReplication(environment.Content)
	}
	artifacts.Direct = cfg.Gateway.DirectTransfer
	artifacts.Limits = gitrepo.Limits{MaxFiles: cfg.Policies.Snapshot.MaxFiles, MaxBytes: cfg.Policies.Snapshot.MaxBytes, MaxFileBytes: cfg.Policies.Snapshot.MaxFileBytes}
	artifacts.Review = gitrepo.ReviewLimits{MaxChanges: cfg.Policies.Review.MaxChanges, MaxDiffBytes: cfg.Policies.Review.MaxDiffBytes, MaxFileBytes: cfg.Policies.Review.MaxFileBytes, MaxEntries: cfg.Policies.Review.MaxEntries, Timeout: time.Duration(cfg.Policies.Review.Timeout)}
	if settings := boot.Settings(); settings != nil {
		artifacts.Policy = func() (gitrepo.Limits, gitrepo.ReviewLimits) {
			p := settings.Load().Policies
			return gitrepo.Limits{MaxFiles: p.Snapshot.MaxFiles, MaxBytes: p.Snapshot.MaxBytes, MaxFileBytes: p.Snapshot.MaxFileBytes},
				gitrepo.ReviewLimits{MaxChanges: p.Review.MaxChanges, MaxDiffBytes: p.Review.MaxDiffBytes, MaxFileBytes: p.Review.MaxFileBytes, MaxEntries: p.Review.MaxEntries, Timeout: time.Duration(p.Review.Timeout)}
		}
	}
	// Side effects agents ask for are intents: claimed, journaled, and
	// blocked across attempts until a person resolves an unknown outcome.
	intents := intent.New(book)
	// A landing the previous process was cut off in is finished — or
	// stopped at a conflict — before any turn can touch the canonical. One
	// that cannot be finished yet stays recovery-pending, which keeps new
	// landings off its project, and the landing sweep retries it; it is
	// never a reason for the generation not to start.
	recovered, err := artifacts.RecoverLandings(ctx)
	if err != nil {
		slog.Error(fmt.Sprintf("steve: recover landings: %v", err), "error", err.Error())
	}
	for _, l := range recovered {
		slog.Info(fmt.Sprintf("steve: recovered landing %s of %s into %s: %s", l.ID, l.Artifact, l.Project, l.State), "landing", l.ID, "artifact", l.Artifact, "project", l.Project)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		return nil, fmt.Errorf("open tasks: %w", err)
	}
	executions := execution.New(ctx, tasks)
	artifacts.SetExecution(executions)
	tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	if settings := boot.Settings(); settings != nil {
		tasks.BudgetSource = func() (int, time.Duration) {
			p := settings.Load().Gateway
			return p.TaskMaxTurns, time.Duration(p.TaskMaxElapsed)
		}
	}
	schedules, err := schedule.OpenLedger(book)
	if err != nil {
		return nil, fmt.Errorf("open schedules: %w", err)
	}
	plans, err := plan.OpenLedger(book)
	if err != nil {
		return nil, fmt.Errorf("open plans: %w", err)
	}
	var channelOwners map[string]string
	if cfg.FeishuEnabled() {
		channelOwners = map[string]string{"feishu": cfg.Feishu.OwnerOpenID}
	}
	timeoutSource, autoResolveSource := turnPolicy(boot.Settings())
	coordinator, err := turn.New(turn.Deps{
		Catalog: catalog, Store: store, Assembler: assembler, Runtime: manager,
		Timeout: time.Duration(cfg.Gateway.PromptTimeout), TimeoutSource: timeoutSource,
		AutoResolveSource: autoResolveSource, Text: catalogText,
		Owner: cfg.EffectiveOwnerID(), ChannelOwners: channelOwners, Home: profile.Home, Skills: live,
		Projects: projects, DefaultProject: cfg.Gateway.DefaultProject, HomeProject: adminsvc.HomeProjectID,
		Memory: memories, Attempts: attempts, Artifacts: artifacts, Intents: intents,
		Executions: executions, Tasks: tasks, Node: boot.NodeName(), Schedules: schedules,
		OfflineAfter: time.Duration(cfg.Gateway.OfflineReminderAfter), ConsoleCompletionGuard: console.CheckTaskCompletionTx,
		Nodes: nodes, PlanRecoveryOwner: planRecoveryOwner(environment != nil),
	})
	if err != nil {
		return nil, err
	}
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

// planRecoveryOwner reports the tasks whose transport resumes its own
// retained plans: the console's, when the console recovers its retained
// exchanges itself, as it does under an Environment.
func planRecoveryOwner(consoleRecovers bool) func(task.Task) bool {
	return func(tracked task.Task) bool { return consoleRecovers && tracked.Transport == "console" }
}

// turnPolicy is the prompt timeout and conflict policy a coordinator reads
// from the latest published settings; both are nil without settings.
func turnPolicy(settings *config.RuntimeSettings) (timeout func() time.Duration, autoResolve func() bool) {
	if settings == nil {
		return nil, nil
	}
	timeout = func() time.Duration { return time.Duration(settings.Load().Gateway.PromptTimeout) }
	autoResolve = func() bool { return settings.Load().Policies.Landing.Conflicts != config.ConflictsManual }
	return timeout, autoResolve
}

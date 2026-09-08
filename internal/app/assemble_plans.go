package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	gopactsqlite "github.com/gopact-ai/gopact-ext/stores/sqlite"
	"github.com/gopact-ai/gopact/workflow"
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/workflowstore"
)

func assemblePlans(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly, modelInfo modelsAssembly, work executionAssembly) (plansAssembly, error) {
	environment := input.Environment()
	book := boot.Book()
	catalog := boot.Catalog()
	cfg := boot.Config()
	manager := boot.Manager()
	attempts := storage.Attempts()
	assembler := identity.Assembler()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	endpoints := modelInfo.Endpoints()
	probeDir := modelInfo.ProbeDir()
	prober := modelInfo.Prober()
	artifacts := work.Artifacts()
	coordinator := work.Coordinator()
	executions := work.Executions()
	plans := work.Plans()
	tasks := work.Tasks()

	// The supervisor is the plan side: a planner decides what should happen,
	// and the runtime holds everything that must be true regardless of who
	// planned it — placement against the live roster, the budget, the
	// verification rule, the recovery limit.
	stepRunner := exec.NewAgentRunner(manager, capabilitiesFor(assembler), fleet)
	stepRunner.Timeout = time.Duration(cfg.Policies.Execution.StepTimeout)
	// Workflow checkpoints are durable so a plan outlives the process that
	// started it; the ledger records which runs are open.
	var checkpoints workflow.Store
	if environment != nil {
		checkpoints = workflowstore.New(book)
	} else {
		workflowsDB := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "workflows.db")
		if err := gopactsqlite.Migrate(workflowsDB); err != nil {
			return nil, fmt.Errorf("migrate workflow checkpoints: %w", err)
		}
		localStore, err := gopactsqlite.Open(workflowsDB)
		if err != nil {
			return nil, fmt.Errorf("open workflow checkpoints: %w", err)
		}
		life.Defer(func() { localStore.Close() })
		checkpoints = localStore
	}

	auxiliary := agentexec.New(manager, fleet, artifacts, attempts, executions, taskBudget{tasks: tasks})
	auxiliary.SetCapabilities(capabilitiesFor(assembler))
	verifiers := exec.NewVerifiers(nodes, auxiliary)
	verifiers.Timeout = time.Duration(cfg.Policies.Execution.VerifyTimeout)
	supervisor := exec.NewSupervisor(
		choosePlanner(cfg, catalog, auxiliary),
		exec.Deps{
			Roster: fleet, Runner: stepRunner, Budget: taskBudget{tasks: tasks},
			// Verification runs where the work is: a command on the step's
			// node, or a second agent asked to check the first one's.
			Verifier:   verifiers,
			Workspaces: artifacts,
			Attempts:   attempts,
			Artifacts:  artifacts,
		},
		checkpoints,
	)
	supervisor.SetExecution(executions)
	supervisor.SetPlans(plans)
	supervisor.SetLedger(book, adminsvc.NodeName())
	supervisor.SetTasks(tasks)
	coordinator.SetSupervisor(supervisor, plans, fleet)
	coordinator.SetPlanRecoveryOwner(func(tracked task.Task) bool { return environment != nil && console.IsConsole(tracked.Channel) })
	coordinator.SetRepair(nodes, nodes)
	coordinator.SetProber(func(ctx context.Context, node, harnessID string) error {
		dir := probeDir(node)
		if dir == "" {
			return fmt.Errorf("no state dir known for %s", nodewire.Place(node))
		}
		_, err := prober.Probe(ctx, models.Endpoint{Node: node, Harness: harnessID, Workdir: dir})
		return err
	}, func(ctx context.Context) []models.Result { return prober.ProbeAll(ctx, endpoints(ctx), true) })
	return &plansValues{auxiliary: auxiliary, stepRunner: stepRunner, supervisor: supervisor}, nil
}

type plansAssembly interface {
	Auxiliary() *agentexec.Runner
	StepRunner() *exec.AgentRunner
	Supervisor() *exec.Supervisor
}

type plansValues struct {
	auxiliary  *agentexec.Runner
	stepRunner *exec.AgentRunner
	supervisor *exec.Supervisor
}

func (v *plansValues) Auxiliary() *agentexec.Runner { return v.auxiliary }

func (v *plansValues) StepRunner() *exec.AgentRunner { return v.stepRunner }

func (v *plansValues) Supervisor() *exec.Supervisor { return v.supervisor }

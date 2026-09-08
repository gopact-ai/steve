package app

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

// capabilitiesFor adapts the capability assembler to what a step runner
// needs. A plan step opens its own session, so it needs the same identity and
// MCP servers an ordinary turn would get on that agent.
func capabilitiesFor(assembler *capability.Assembler) exec.Capabilities {
	return assembledCaps{assembler: assembler}
}

type assembledCaps struct{ assembler *capability.Assembler }

func (a assembledCaps) Assemble(candidate roster.Candidate) (string, []acp.MCPServer, error) {
	// Guest mode: a plan step is work, not a conversation with the owner, so
	// it gets the shared identity rather than the owner's private home.
	caps, err := a.assembler.AssembleMode(candidate.Agent, home.ModeGuest)
	if err != nil {
		return "", nil, err
	}
	return caps.Instructions + exec.ReportingContract, caps.MCPServers, nil
}

// taskBudget is the plan's brake. It charges each step to the task that owns
// the plan, so a plan cannot spend more than the work it belongs to was
// allowed — the same budget a chat turn is held to.
type taskBudget struct{ tasks *task.Store }

func (b taskBudget) ReserveAttempt(record attempt.Record) (int, time.Time, error) {
	if record.Execution == nil {
		return 0, time.Time{}, errors.New("budget reservation needs the original execution token")
	}
	tracked, err := b.tasks.ReserveAttempt(*record.Execution, record.ID, record.TurnID, record.Agent, record.Node, record.StartedAt)
	if err != nil {
		return 0, time.Time{}, err
	}
	left := max(0, tracked.Budget.MaxTurns-tracked.Budget.Turns)
	var deadline time.Time
	if tracked.Budget.MaxElapsed > 0 {
		deadline = time.Now().Add(tracked.Budget.MaxElapsed - tracked.Budget.Elapsed)
	}
	return left, deadline, nil
}

func (b taskBudget) SettleAttempt(record attempt.Record, outcome task.Outcome) error {
	return b.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome, stoppedAccounting(record))
}

func (b taskBudget) Reserve(taskID string) (int, time.Time, error) {
	tracked, err := b.tasks.ReserveTurn(taskID)
	if err != nil {
		return 0, time.Time{}, err
	}
	left := max(0, tracked.Budget.MaxTurns-tracked.Budget.Turns)
	var deadline time.Time
	if tracked.Budget.MaxElapsed > 0 {
		deadline = time.Now().Add(tracked.Budget.MaxElapsed - tracked.Budget.Elapsed)
	}
	return left, deadline, nil
}

// choosePlanner is the one place the planning strategy is picked. A
// configured planning agent decomposes open goals with a model; without one
// the rule planner places declared steps and treats an open goal as a single
// step. Both produce the same validated Plan and the executor cannot tell
// which one did.
func choosePlanner(cfg *config.Config, catalog *agent.Catalog, executor *agentexec.Runner) planner.Planner {
	if cfg.Gateway.Planner == "" {
		return planner.Rule{}
	}
	selected, ok := catalog.Resolve(cfg.Gateway.Planner)
	if !ok {
		slog.Warn(fmt.Sprintf("steve: planner agent %q not in catalog; using the rule planner", cfg.Gateway.Planner), "agent", cfg.Gateway.Planner)
		return planner.Rule{}
	}
	slog.Info(fmt.Sprintf("steve: /plan decomposes with %s", selected.ID), "agent", selected.ID)
	return planner.LLM{
		Agent: selected.ID, Executor: executor, Timeout: time.Duration(cfg.Policies.Planning.Timeout), Attempts: cfg.Policies.Planning.Attempts,
	}
}

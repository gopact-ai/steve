package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agentmcp"
	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	steveview "github.com/gopact-ai/steve/internal/view"
)

func assembleDelegation(life lifetime, input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly, work executionAssembly, projection readModelAssembly, page consoleAssembly) (delegationAssembly, error) {
	environment := input.Environment()
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	manager := boot.Manager()
	attempts := storage.Attempts()
	assembler := identity.Assembler()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	artifacts := work.Artifacts()
	executions := work.Executions()
	gw := work.Gateway()
	tasks := work.Tasks()
	view := projection.View()
	admin := page.Admin()
	cons := page.Console()

	// The messaging server's port is remembered across restarts only so it
	// rarely moves. A moved port is not a configuration change: session
	// fingerprints leave it out, and a resumed session is given the new URL.
	// The page recognises the platform's own tool calls by the messaging
	// server's name and catalogue, whatever a harness calls them.
	readmodel.SetPlatformTools(agentmcp.ServerName, agentmcp.ToolTitles())
	portPath := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "agentmcp.port")
	gate, err := agentmcp.New(readPort(portPath))
	// redeliverPending is the delegation service's start-up pass, once it exists.
	var reconcileDeliveries func(context.Context) error
	var recoverRetainedDelegates func(context.Context) error
	// messaging stays zero when the messaging server could not bind: a nil
	// *agentmcp.Server must not reach the coordinator as a non-nil gate.
	var messaging messagingCallbacks
	if err != nil {
		// The send primitive is an enhancement; a box that cannot bind a
		// loopback port still serves ordinary turns.
		slog.Warn(fmt.Sprintf("steve: agent messaging disabled: %v", err))
		gate = nil
	} else {
		// The port is bound from here on. Closing drains the tool calls in
		// flight, so it is registered after the ledger and the services the
		// tools reach, and runs before they close; it also gives the port
		// back when a later assembly step fails before serving starts.
		life.Defer(gate.Close)
		if environment != nil {
			if err := gate.SetStore(newApplicationMCPStore(book), environment.Fail); err != nil {
				return nil, fmt.Errorf("configure shared collaboration tools: %w", err)
			}
		}
		if err := os.WriteFile(portPath, []byte(fmt.Sprintf("%d\n", gate.Port())), 0o600); err != nil {
			slog.Error(fmt.Sprintf("steve: remember agent messaging port: %v", err))
		}
		// Delegation is the one way an agent reaches another: a child task
		// in the tree, funded from the caller's remainder, with its own
		// token. It is offered only when the messaging server exists,
		// because that is where the tool lives.
		delegation := delegate.New(tasks, fleet, manager, assembler, artifacts, boot.NodeName())
		delegation.SetExecution(executions)
		delegation.SetLedger(attempts, artifacts)
		delegation.SetGate(gate)
		delegation.SetEndpoints(nodes)
		wireDelegateQuestions(delegation, cons)
		if environment != nil {
			wireDelegateRecovery(delegation, cons)
			delegation.SetSpawnGuard(func(ctx context.Context, tx *ledger.Tx) error {
				return agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx})
			})
		}
		// Children an earlier process left behind have no other owner, with
		// or without a cluster: a standalone hub restarts too, and a child
		// row it opened before dying stays open until this pass settles it.
		recoverRetainedDelegates = delegation.RecoverRetained
		delegation.MaxSilence = time.Duration(cfg.Gateway.PromptTimeout)
		if settings := boot.Settings(); settings != nil {
			delegation.SilenceSource = func() time.Duration { return time.Duration(settings.Load().Gateway.PromptTimeout) }
		}
		delegation.RecoveryQuiet = time.Duration(cfg.Gateway.RecoveryQuiet)
		delegation.RegisterIdle = nodes.RegisterIdle
		delegation.SetObserver(delegateObserver(ctx, boot.NodeName(), admin, view, cons))
		gate.SetDelegator(delegation)
		wireDelegateDelivery(delegation, cons, gw)
		messaging = messagingCallbacks{
			AgentGate: gate,
			AfterTurn: func(taskID string) { delegation.Flush(ctx, taskID) },
			TurnPreface: func(ctx context.Context, taskID string) turn.Preface {
				text, told := delegation.Preface(ctx, taskID)
				return turn.Preface{Text: text, Told: told}
			},
		}
		reconcileDeliveries = delegation.ReconcileDeliveries
		// Remote agents call a loopback port on their own machine; the node
		// forwards it back here over the connection it already holds, so the
		// messaging server never has to leave 127.0.0.1.
		nodes.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", gate.Addr())
		})
		gw.SetAgentGate(gate)
		gate.SetJournal(func(_, _, taskID string, receipt messagechannel.Address) {
			// The existing Feishu revival flow keeps its native message receipts.
			if receipt.Channel != "feishu" {
				return
			}
			messageID := receipt.Message
			if err := tasks.AddInterimForTask(taskID, messageID); err != nil {
				slog.Error(fmt.Sprintf("steve: journal interim message: %v", err), "task", taskID)
			}
		})
	}
	return &delegationValues{gate: gate, messaging: messaging, recoverRetainedDelegates: recoverRetainedDelegates, reconcileDeliveries: reconcileDeliveries}, nil
}

type delegationAssembly interface {
	Gate() *agentmcp.Server
	Messaging() messagingCallbacks
	RecoverRetainedDelegates() func(context.Context) error
	ReconcileDeliveries() func(context.Context) error
}

// messagingCallbacks are the coordinator's AgentGate, AfterTurn and
// TurnPreface, all nil when the messaging server could not bind.
type messagingCallbacks struct {
	AgentGate   turn.AgentGate
	AfterTurn   func(taskID string)
	TurnPreface func(ctx context.Context, taskID string) turn.Preface
}

type delegationValues struct {
	gate                     *agentmcp.Server
	messaging                messagingCallbacks
	recoverRetainedDelegates func(context.Context) error
	reconcileDeliveries      func(context.Context) error
}

func (v *delegationValues) Gate() *agentmcp.Server { return v.gate }

// startMessaging serves the messaging MCP server, when there is one. Its
// tools reach the coordinator, so it starts only once assembly is done.
func startMessaging(boot runtimeAssembly, delegates delegationAssembly) {
	gate := delegates.Gate()
	if gate == nil {
		return
	}
	boot.Background().Go(func(ctx context.Context) {
		if err := gate.Start(ctx); err != nil {
			slog.Error(fmt.Sprintf("steve: %v", err))
		}
	})
	slog.Info(fmt.Sprintf("steve: agent messaging MCP server on %s", gate.URL()))
}

func (v *delegationValues) Messaging() messagingCallbacks { return v.messaging }

func (v *delegationValues) RecoverRetainedDelegates() func(context.Context) error {
	return v.recoverRetainedDelegates
}

func (v *delegationValues) ReconcileDeliveries() func(context.Context) error {
	return v.reconcileDeliveries
}

// readPort reads a previously remembered loopback port; 0 means none.
func readPort(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

func delegateObserver(ctx context.Context, hub string, admin *adminsvc.Service, view *readmodel.Model, cons *console.Service) func(delegate.Child, steveview.Progress) {
	return func(c delegate.Child, p steveview.Progress) {
		where := c.Node
		if where == "" {
			where = hub // the hub itself, named like any machine
		}
		info := consoleapi.StepInfo{Kind: "delegate", Goal: c.Goal, State: c.State, Since: c.Since.UTC().Format(time.RFC3339),
			Elapsed: c.Elapsed.Round(time.Second).String(), Answer: c.Answer, Refs: c.Refs}
		if c.State != task.StateRunning && c.Attempt != "" {
			if changes, err := admin.Changes(ctx, c.Attempt); err == nil && changes != nil {
				info.Attempt, info.Files = changes.Attempt, changes.Files
			}
		}
		view.DelegateProgress(c.Task, c.Agent, where, info, p)
		if c.Transport == "console" {
			progress := readmodel.FromProgress(p)
			progress.Agent, progress.Node = c.Agent, where
			cons.UpdateStep(c.Conversation, c.Task, readmodel.FromStepProgress("#"+c.Task, progress, info))
		}
	}
}

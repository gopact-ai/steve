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
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	steveview "github.com/gopact-ai/steve/internal/view"
)

func assembleDelegation(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, machines fleetAssembly, work executionAssembly, projection readModelAssembly, page consoleAssembly) (delegationAssembly, error) {
	environment := input.Environment()
	background := boot.Background()
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	manager := boot.Manager()
	attempts := storage.Attempts()
	assembler := identity.Assembler()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	artifacts := work.Artifacts()
	coordinator := work.Coordinator()
	executions := work.Executions()
	gw := work.Gateway()
	tasks := work.Tasks()
	view := projection.View()
	admin := page.Admin()
	cons := page.Console()

	// The messaging server's URL is baked into session fingerprints, so the
	// port is remembered across restarts: losing it would ask every live
	// conversation for /new after each deploy.
	// The page recognises the platform's own tool calls by the messaging
	// server's name and catalogue, whatever a harness calls them.
	readmodel.SetPlatformTools(agentmcp.ServerName, agentmcp.ToolTitles())
	portPath := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "agentmcp.port")
	gate, err := agentmcp.New(readPort(portPath))
	// redeliverPending is the delegation service's start-up pass, once it exists.
	var redeliverPending func(context.Context)
	var recoverRetainedDelegates func(context.Context) error
	if err != nil {
		// The send primitive is an enhancement; a box that cannot bind a
		// loopback port still serves ordinary turns.
		slog.Warn(fmt.Sprintf("steve: agent messaging disabled: %v", err))
		gate = nil
	} else {
		if environment != nil {
			if err := gate.SetStore(newApplicationMCPStore(book), environment.Fail); err != nil {
				return nil, fmt.Errorf("configure shared collaboration tools: %w", err)
			}
		}
		if err := os.WriteFile(portPath, []byte(fmt.Sprintf("%d\n", gate.Port())), 0o600); err != nil {
			slog.Error(fmt.Sprintf("steve: remember agent messaging port: %v", err))
		}
		coordinator.SetAgentGate(gate)
		coordinator.SetNodeEndpoints(nodes)
		coordinator.RegisterIdle = nodes.RegisterIdle
		// Delegation is the one way an agent reaches another: a child task
		// in the tree, funded from the caller's remainder, with its own
		// token. It is offered only when the messaging server exists,
		// because that is where the tool lives.
		delegation := delegate.New(tasks, fleet, manager, assembler, artifacts, adminsvc.NodeName())
		delegation.SetExecution(executions)
		delegation.SetLedger(attempts, artifacts)
		delegation.SetGate(gate)
		delegation.SetEndpoints(nodes)
		if environment != nil {
			wireDelegateQuestions(delegation, cons)
			delegation.SetSpawnGuard(func(ctx context.Context, tx *ledger.Tx) error {
				return agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx})
			})
			recoverRetainedDelegates = delegation.RecoverRetained
		}
		delegation.MaxSilence = time.Duration(cfg.Gateway.PromptTimeout)
		delegation.RegisterIdle = nodes.RegisterIdle
		delegation.SetObserver(func(c delegate.Child, p steveview.Progress) {
			where := c.Node
			if where == "" {
				where = adminsvc.NodeName() // the hub itself, named like any machine
			}
			info := consoleapi.StepInfo{Kind: "delegate", Goal: c.Goal, State: c.State, Since: c.Since.UTC().Format(time.RFC3339),
				Elapsed: c.Elapsed.Round(time.Second).String(), Answer: c.Answer, Refs: c.Refs}
			if c.State != task.StateRunning && c.Attempt != "" {
				if changes, err := admin.Changes(ctx, c.Attempt); err == nil && changes != nil {
					info.Attempt, info.Files = changes.Attempt, changes.Files
				}
			}
			view.DelegateProgress(c.Task, c.Agent, where, info, p)
			if console.IsConsole(c.Conversation) {
				progress := readmodel.FromProgress(p)
				progress.Agent, progress.Node = c.Agent, where
				cons.UpdateStep(c.Conversation, c.Task, readmodel.FromStepProgress("#"+c.Task, progress, info))
			}
		})
		gate.SetDelegator(delegation)
		// A child's result goes back into its parent's conversation as a
		// message — the page's queue or the chat — instead of the parent
		// polling for it; a turn's end delivers what ended meanwhile.
		delegation.SetDeliverer(func(ctx context.Context, d delegate.Delivery) error {
			if d.ChatID == console.ChatID || console.IsConsole(d.Conversation) {
				return cons.Continue(ctx, d.Conversation, d.Key, d.Member, d.Notice(), d.Prompt())
			}
			return gw.Deliver(gateway.Revival{TaskID: d.ParentTask, Member: d.Member, ConversationID: d.Conversation,
				ChatID: d.ChatID, MessageID: d.Anchor, Requester: d.Requester, ChatType: d.ChatType}, d.Notice(), d.Prompt())
		})
		coordinator.SetAfterTurn(func(taskID string) { delegation.Flush(ctx, taskID) })
		redeliverPending = delegation.RedeliverPending
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
		background.Go(func(ctx context.Context) {
			if err := gate.Start(ctx); err != nil {
				slog.Error(fmt.Sprintf("steve: %v", err))
			}
		})
		slog.Info(fmt.Sprintf("steve: agent messaging MCP server on %s", gate.URL()))
	}
	return &delegationValues{gate: gate, recoverRetainedDelegates: recoverRetainedDelegates, redeliverPending: redeliverPending}, nil
}

type delegationAssembly interface {
	Gate() *agentmcp.Server
	RecoverRetainedDelegates() func(context.Context) error
	RedeliverPending() func(context.Context)
}

type delegationValues struct {
	gate                     *agentmcp.Server
	recoverRetainedDelegates func(context.Context) error
	redeliverPending         func(context.Context)
}

func (v *delegationValues) Gate() *agentmcp.Server { return v.gate }

func (v *delegationValues) RecoverRetainedDelegates() func(context.Context) error {
	return v.recoverRetainedDelegates
}

func (v *delegationValues) RedeliverPending() func(context.Context) { return v.redeliverPending }

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

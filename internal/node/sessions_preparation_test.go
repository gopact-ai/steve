package node

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type lostPreparationReply struct {
	*Registry
	dropped bool
	native  string
}

// A native open that never reached the node created nothing there, so it
// fails without holding the execution; a resume names a native session
// that already exists, and not reaching the node says nothing about it.
func TestNodePreparationUnreachableNodeDoesNotClaimNativeOpenWasDispatched(t *testing.T) {
	refused := errors.New("connection refused")
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: "worker.example:7701", Token: "unreachable-session", DialContext: func(context.Context, string) (net.Conn, error) {
		return nil, refused
	}}})
	defer registry.Close()
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	defer manager.Stop()
	request := nodeSessionRequest("open")
	ctx := harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "never-dispatched"})
	_, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
	var unsent *nodewire.SessionNotDispatched
	var uncertain *harness.NodeSessionOpenUncertain
	if !errors.As(err, &unsent) || !errors.Is(err, refused) || errors.As(err, &uncertain) || errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("open that never reached the node = %v, want it failed without holding the execution", err)
	}
	resumed := "ns_" + strings.Repeat("0", 64)
	_, err = manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, resumed, t.TempDir(), nil)
	if !errors.As(err, &uncertain) || !errors.Is(err, harness.ErrStopUnconfirmed) || uncertain.SessionID != resumed {
		t.Fatalf("resume that never reached the node = %v, want the existing native session held as uncertain", err)
	}
}

// A node that runs without node sessions refuses an open before it holds
// anything, so the hub fails the open without holding the execution.
func TestNodePreparationNodeWithoutSessionsRefusesOpenAsNotStarted(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "no-sessions", StateDir: t.TempDir()})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "no-sessions"}})
	defer registry.Close()
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	defer manager.Stop()
	request := nodeSessionRequest("open")
	ctx := harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "refused-by-node"})
	_, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
	var refused *nodewire.SessionOpenNotStarted
	var uncertain *harness.NodeSessionOpenUncertain
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "node sessions are not enabled") || errors.As(err, &uncertain) || errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("open refused by a node without sessions = %v, want it failed without holding the execution", err)
	}
}

func (r *lostPreparationReply) NodeSession(ctx context.Context, node string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	state, err := r.Registry.NodeSession(ctx, node, request)
	if err == nil && request.Action == "open" && !r.dropped {
		r.dropped, r.native = true, state.ID
		return nodewire.SessionState{}, io.ErrUnexpectedEOF
	}
	return state, err
}

func TestNodePreparationReusesFrozenMCPPayloadAfterOpenResponseIsLost(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	server := startNode(t, ServerConfig{Name: "worker", Token: "prepared-open", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "prepared-open"}})
	defer registry.Close()
	transport := &lostPreparationReply{Registry: registry}
	first, _ := harness.NewManager(nil)
	first.SetTransports(transport)
	defer first.Stop()
	request := nodeSessionRequest("open")
	binding := harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "original-input"}
	ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), binding), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	servers := []acp.MCPServer{
		acp.HTTPMCPServer("http-fixture", "http://127.0.0.1:1/b/original-binding", nil),
		acp.StdioMCPServer("stdio-fixture", "/not-executed-preparation-fixture", []string{"original-binding"}, nil),
	}
	_, err := first.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", workspace, servers)
	var uncertain *harness.NodeSessionOpenUncertain
	if !errors.As(err, &uncertain) || !errors.Is(err, harness.ErrStopUnconfirmed) || !errors.Is(err, harness.ErrNodeSessionUnavailable) || uncertain.Binding != request.Binding || uncertain.OpenCommandID != "original-input/open" || uncertain.SessionID != "" {
		t.Fatalf("lost native open did not retain its exact preparation identity: %+v %v", uncertain, err)
	}
	if transport.native == "" {
		t.Fatal("fixture did not create the original native session")
	}
	first.Stop()
	registry.Close()
	authority.mu.Lock()
	authority.epoch, authority.writer = 2, 2
	authority.mu.Unlock()
	binding.Authority.CoordinatorNodeID, binding.Authority.CoordinatorEpoch, binding.Authority.WriterGeneration = "hub-b", 2, 2
	nextCtx, finishNext := context.WithTimeout(harness.WithNodeSession(t.Context(), binding), 10*time.Second)
	defer finishNext()
	nextRegistry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "prepared-open"}})
	defer nextRegistry.Close()
	next, _ := harness.NewManager(nil)
	next.SetTransports(nextRegistry)
	defer next.Stop()
	// No ns ID was returned to the first coordinator. Reuse the original
	// open command and byte-equivalent frozen MCP configuration.
	runner, err := next.OpenSession(nextCtx, harness.Placement{Node: "worker", Harness: "mock"}, "", workspace, servers)
	if err != nil || runner.ID() != transport.native {
		t.Fatalf("preparation retry replaced native session: %v %v", runner, err)
	}
	changed := append([]acp.MCPServer(nil), servers...)
	changed[0] = acp.HTTPMCPServer("http-fixture", "http://127.0.0.1:1/b/new-binding", nil)
	if _, err := next.OpenSession(nextCtx, harness.Placement{Node: "worker", Harness: "mock"}, "", workspace, changed); err == nil {
		t.Fatal("same open command silently rebound a new MCP identity")
	}
	output, _, err := runner.Prompt(nextCtx, "continue once", nil)
	if err != nil || !strings.Contains(output, "continue once") {
		t.Fatalf("original prepared session did not continue: %q %v", output, err)
	}
	state, err := runner.(harness.RetainedSessionInspector).InspectRetained(nextCtx)
	if err != nil || state.InputAccepted != 1 || state.Command == nil || state.Command.ID != "original-input" || !state.Command.Settled {
		t.Fatalf("preparation recovery replayed or replaced original input: %+v %v", state, err)
	}
}

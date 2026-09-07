package node

import (
	"context"
	"errors"
	"io"
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

func TestNodePreparationUnsupportedNodeDoesNotClaimNativeOpenWasDispatched(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "unsupported-session", StateDir: t.TempDir()})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "unsupported-session"}})
	defer registry.Close()
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	defer manager.Stop()
	request := nodeSessionRequest("open")
	ctx := harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "never-dispatched"})
	_, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
	var uncertain *harness.NodeSessionOpenUncertain
	if err == nil || errors.As(err, &uncertain) || errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("unsupported capability falsely quarantined an execution that never opened: %v", err)
	}
	_, err = manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "ns_"+strings.Repeat("0", 64), t.TempDir(), nil)
	if !errors.As(err, &uncertain) || !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("unsupported capability invented safety for a previously existing native session: %v", err)
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

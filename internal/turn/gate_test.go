package turn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/state"
)

type fakeGate struct {
	mu    sync.Mutex
	calls []string // conversation:agent:token
	// endpoints records the URL each session was told to call, so a test
	// can assert a remote agent got its own node's loopback address.
	endpoints []string
}

func (g *fakeGate) Extras(conversationID, agentID, token, endpoint string) []capability.Extra {
	if endpoint == "" {
		endpoint = "http://127.0.0.1:1/mcp"
	}
	g.mu.Lock()
	g.calls = append(g.calls, conversationID+":"+agentID+":"+token)
	g.endpoints = append(g.endpoints, endpoint)
	g.mu.Unlock()
	return []capability.Extra{{
		Name: "feishu",
		Server: capability.MCPServer{
			Type: "http", URL: endpoint,
			Headers: map[string]string{"Authorization": "Bearer " + token},
		},
		Instructions: "GATE-RULES: send milestones sparingly",
	}}
}

func (g *fakeGate) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func gateCoordinator(t *testing.T, mcpHTTP bool) (*Coordinator, *fakeManager, *fakeRunner, *fakeGate, *state.Store) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}, mcpHTTP: mcpHTTP}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	gate := &fakeGate{}
	coordinator.SetAgentGate(gate)
	return coordinator, manager, runner, gate, store
}

func TestCoordinatorInjectsGateForHTTPMCPHarness(t *testing.T) {
	coordinator, manager, runner, gate, store := gateCoordinator(t, true)
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	session := store.Conversation("chat").Sessions["codex"]
	if session.AgentToken == "" {
		t.Fatal("no agent token persisted with the session")
	}
	if len(manager.servers) != 1 || len(manager.servers[0]) != 1 {
		t.Fatalf("gate server not injected: %#v", manager.servers)
	}
	injected := manager.servers[0][0]
	if injected.Name != "feishu" || len(injected.Headers) != 1 ||
		injected.Headers[0].Value != "Bearer "+session.AgentToken {
		t.Fatalf("injected server does not carry the session token: %#v", injected)
	}
	if prompts := runner.seen(); len(prompts) != 1 || !strings.Contains(prompts[0], "GATE-RULES") {
		t.Fatalf("gate instructions not injected: %q", prompts)
	}
	// The second turn reuses the token, so the capability fingerprint stays
	// stable and the session survives.
	if _, err := handle(coordinator, t.Context(), "again"); err != nil {
		t.Fatalf("second turn hit capability drift: %v", err)
	}
	after := store.Conversation("chat").Sessions["codex"]
	if after.AgentToken != session.AgentToken {
		t.Fatalf("token drifted between turns: %q -> %q", session.AgentToken, after.AgentToken)
	}
	calls := gate.seen()
	if len(calls) != 2 || calls[0] != calls[1] || !strings.HasPrefix(calls[0], "chat:codex:") {
		t.Fatalf("gate calls drifted: %v", calls)
	}
}

func TestCoordinatorSkipsGateWhenHarnessLacksHTTPMCP(t *testing.T) {
	coordinator, manager, _, gate, store := gateCoordinator(t, false)
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if calls := gate.seen(); len(calls) != 0 {
		t.Fatalf("gate consulted for a harness without HTTP MCP: %v", calls)
	}
	if len(manager.servers) != 1 || len(manager.servers[0]) != 0 {
		t.Fatalf("servers injected anyway: %#v", manager.servers)
	}
	if token := store.Conversation("chat").Sessions["codex"].AgentToken; token != "" {
		t.Fatalf("token minted for a harness that cannot use it: %q", token)
	}
}

func TestCoordinatorMintsDistinctTokensPerConversation(t *testing.T) {
	coordinator, _, _, _, store := gateCoordinator(t, true)
	for _, conversation := range []string{"chat-a", "chat-b"} {
		if _, err := coordinator.Handle(t.Context(), Request{ConversationID: conversation, Input: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	tokenA := store.Conversation("chat-a").Sessions["codex"].AgentToken
	tokenB := store.Conversation("chat-b").Sessions["codex"].AgentToken
	if tokenA == "" || tokenB == "" || tokenA == tokenB {
		t.Fatalf("conversation tokens not distinct: %q vs %q", tokenA, tokenB)
	}
}

// fakeEndpoints answers with a node-local loopback URL, the way the node
// registry does from a node's advert.
type fakeEndpoints struct {
	port map[string]int
	fail error
}

func (f fakeEndpoints) MCPEndpoint(_ context.Context, node string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	port, ok := f.port[node]
	if !ok {
		return "", errors.New("unknown node " + node)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil
}

// TestRemoteAgentGetsItsOwnNodeLoopback: the messaging URL handed to an agent
// must be reachable from the machine that agent runs on. The hub's own
// loopback address means nothing over there.
func TestRemoteAgentGetsItsOwnNodeLoopback(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
		"lab":   {Harness: "codex", Node: "host-3", Aliases: []string{"lab"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}, mcpHTTP: true}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	gate := &fakeGate{}
	coordinator.SetAgentGate(gate)
	coordinator.SetNodeEndpoints(fakeEndpoints{port: map[string]int{"host-3": 45999}})

	if _, err := handle(coordinator, t.Context(), "/project use lab"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "@lab go"); err != nil {
		t.Fatal(err)
	}
	if len(gate.endpoints) != 1 || gate.endpoints[0] != "http://127.0.0.1:45999/mcp" {
		t.Fatalf("remote endpoint = %v, want the node's own loopback port", gate.endpoints)
	}
	if _, err := handle(coordinator, t.Context(), "/project use codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "@codex go"); err != nil {
		t.Fatal(err)
	}
	if gate.endpoints[1] != "http://127.0.0.1:1/mcp" {
		t.Fatalf("hub-local endpoint = %q, want the hub's own URL", gate.endpoints[1])
	}
}

// TestUnreachableNodeMessagingCostsTheCapabilityNotTheTurn: a node that
// cannot forward messaging loses the milestone cards, but the turn still
// runs and still answers.
func TestUnreachableNodeMessagingCostsTheCapabilityNotTheTurn(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"lab": {Harness: "codex", Node: "host-3", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "still answered"}}, mcpHTTP: true}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	gate := &fakeGate{}
	coordinator.SetAgentGate(gate)
	coordinator.SetNodeEndpoints(fakeEndpoints{fail: errors.New("node down")})

	result, err := handle(coordinator, t.Context(), "go")
	if err != nil {
		t.Fatalf("turn failed because messaging was unavailable: %v", err)
	}
	if result.Text != "still answered" {
		t.Fatalf("result = %q", result.Text)
	}
	if len(gate.endpoints) != 0 {
		t.Fatalf("messaging should have been skipped, got %v", gate.endpoints)
	}
}

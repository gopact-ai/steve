package turn

import (
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
}

func (g *fakeGate) Extras(conversationID, agentID, token string) []capability.Extra {
	g.mu.Lock()
	g.calls = append(g.calls, conversationID+":"+agentID+":"+token)
	g.mu.Unlock()
	return []capability.Extra{{
		Name: "feishu",
		Server: capability.MCPServer{
			Type: "http", URL: "http://127.0.0.1:1/mcp",
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
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
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
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
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

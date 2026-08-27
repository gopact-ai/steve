package turn

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
)

type e2eSender struct {
	mu      sync.Mutex
	cards   []string // anchor:id:payload
	deleted []string
	next    int
}

func (f *e2eSender) ReplyCard(_ context.Context, messageID string, payload []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("om_sent_%d", f.next)
	f.cards = append(f.cards, messageID+":"+id+":"+string(payload))
	return id, nil
}

func (f *e2eSender) ReplyText(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("unexpected text send")
}

func (f *e2eSender) DeleteMessage(_ context.Context, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return nil
}

// TestAgentSendPrimitiveE2E drives the whole wire: coordinator → ACP host →
// mockagent subprocess → HTTP MCP call back into the gateway's messaging
// server → (fake) Feishu channel. It pins the shape botmux got by trusting
// the model: here delivery of milestones is a tool, the final answer stays
// with the platform.
func TestAgentSendPrimitiveE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	gate, err := agentmcp.New()
	if err != nil {
		t.Fatal(err)
	}
	sender := &e2eSender{}
	gate.BindChannel(sender)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := gate.Start(ctx); err != nil {
			t.Errorf("gate: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})

	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"mock": {Harness: "mock", Workspace: t.TempDir(), Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(map[string]harness.Config{
		"mock": {Command: bin, Permission: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	coordinator.SetAgentGate(gate)
	gate.Anchor("chat", "oc_chat", "om_user_1")

	result, err := handle(coordinator, context.Background(), "mcpfull now")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "mention_rejected=true") || !strings.Contains(result.Text, "recalled_ok=true") {
		t.Fatalf("mcp flow did not complete: %q", result.Text)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.cards) != 1 || !strings.HasPrefix(sender.cards[0], "om_user_1:") {
		t.Fatalf("milestone card not delivered to the turn anchor: %v", sender.cards)
	}
	if !strings.Contains(sender.cards[0], "phase one done") {
		t.Fatalf("card payload wrong: %v", sender.cards)
	}
	if len(sender.deleted) != 1 || sender.deleted[0] != "om_sent_1" {
		t.Fatalf("recall did not delete the sent card: %v", sender.deleted)
	}
	if token := store.Conversation("chat").Sessions["mock"].AgentToken; token == "" {
		t.Fatal("no token persisted for the session")
	}
}

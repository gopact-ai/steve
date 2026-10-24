package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/pihome"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

// A pi started through the raw process stream runs in the home the hub's
// policy needs. The node's own PI_CODING_AGENT_DIR never wins.
func TestProcessStreamStartsPiUnderTheHubPolicy(t *testing.T) {
	bin := buildMockAgent(t)
	root := t.TempDir()
	t.Setenv("HOME", root)
	seen := filepath.Join(root, "seen")
	adapter := filepath.Join(root, "pi-acp")
	script := "#!/bin/sh\necho \"$PI_CODING_AGENT_DIR $PI_ACP_PI_COMMAND $STEVE_PI_REAL\" >> '" + seen + "'\nexec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(adapter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{Name: "worker", Token: "test-token", StateDir: filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "work"), Harnesses: map[string]HarnessSpec{
		"pi": {Command: adapter, Env: []string{"PI_CODING_AGENT_DIR=" + filepath.Join(root, ".pi", "agent"), "PI_ACP_PI_COMMAND=/opt/pi"}},
	}}
	server := startNode(t, cfg)
	registry := NewRegistry("coordinator", map[string]Config{"worker": {Addr: server.Addr(), Token: "test-token"}})
	t.Cleanup(registry.Close)
	homes := steveruntime.PiHome(cfg.StateDir)
	for i, policy := range []string{permission.PolicyRead, "write"} {
		broker, _ := permission.New(policy)
		host := acphost.New(acphost.Config{Transport: registry.PolicyTransport("worker", "pi", policy), Permission: broker})
		if _, _, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: cfg.WorkspaceRoot}); err != nil {
			t.Fatal(err)
		}
		host.Stop()
		raw, _ := os.ReadFile(seen)
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		dir := pihome.Dir(homes, pihome.Class(policy))
		if len(lines) != i+1 || lines[i] != dir+" "+filepath.Join(dir, "steve-pi")+" /opt/pi" {
			t.Fatalf("%s started pi with %q", policy, lines)
		}
		if pihome.Enforced(dir) != pihome.Class(policy) {
			t.Fatalf("%s home not enforced", policy)
		}
	}
	// An older hub that names no policy gets the read-only home.
	host := acphost.New(acphost.Config{Transport: registry.Transport("worker", "pi"), Permission: mustBroker(t, permission.PolicyRead)})
	t.Cleanup(host.Stop)
	if _, _, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: cfg.WorkspaceRoot}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(seen)
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); !strings.HasPrefix(lines[len(lines)-1], pihome.Dir(homes, pihome.ReadOnly)+" ") {
		t.Fatalf("policy-less stream started pi with %q", lines)
	}
}

func mustBroker(t *testing.T, policy string) *permission.Broker {
	t.Helper()
	broker, err := permission.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

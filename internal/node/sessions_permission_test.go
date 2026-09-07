package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
)

func TestNodeSessionPermissionWithoutOptionalToolKindCanBeAnsweredOnce(t *testing.T) {
	bin := buildMockAgent(t)
	for _, allow := range []bool{false, true} {
		name := "reject"
		if allow {
			name = "allow"
		}
		t.Run(name, func(t *testing.T) {
			server := startNode(t, ServerConfig{Name: "worker", Token: "optional-kind", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
			registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "optional-kind"}})
			defer registry.Close()
			manager, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			manager.SetTransports(registry)
			defer manager.Stop()
			req := nodeSessionRequest("open")
			ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: req.Authority, Binding: req.Binding, CommandID: "permission-input"}), 10*time.Second)
			defer cancel()
			runner, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			asked := 0
			output, _, err := runner.(harness.TurnRunner).PromptTurn(ctx, "perm", nil, func(_ context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
				asked++
				if ask.Kind != acp.ToolKindOther {
					t.Errorf("omitted permission tool kind was not normalized: %q", ask.Kind)
				}
				return permission.Choose(allow, ask.Options), nil
			}, nil, nil)
			if err != nil || asked != 1 || !strings.Contains(output, "[permission: selected/"+name+"]") {
				t.Fatalf("optional kind detached or repeated permission: asks=%d output=%q err=%v", asked, output, err)
			}
			state, err := runner.(harness.RetainedSessionInspector).InspectRetained(ctx)
			if err != nil || state.Command == nil || !state.Command.Settled || len(state.Questions) != 1 || state.Questions[0].Answer == nil || state.Questions[0].Answer.Choice != name {
				t.Fatalf("permission answer not durably settled: %+v %v", state, err)
			}
			saved, exists, err := server.sessions.readRecord(state.ID)
			if err != nil || !exists || len(saved.State.Questions) != 1 || saved.State.Questions[0].Permission == nil || saved.State.Questions[0].Permission.Kind != acp.ToolKindOther || !saved.Commands["permission-input"].Settled {
				t.Fatalf("permission receipt cannot be decoded for node restart: exists=%t error=%v", exists, err)
			}
		})
	}
}

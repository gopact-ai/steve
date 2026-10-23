package node

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestWorkerPluginCloseUsesPluginAuthorityAndConfirmsNativeExit(t *testing.T) {
	bin := buildMockAgent(t)
	s := startNode(t, ServerConfig{Name: "worker", Token: "plugin", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: CoordinatorSessionAuthorizer{}, PluginAuthorizer: CoordinatorPluginAuthorizer{}})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: s.Addr(), Token: "plugin"}})
	t.Cleanup(registry.Close)
	var allowed atomic.Bool
	allowed.Store(true)
	registry.SetPluginAuthorizer(func(context.Context, string, nodewire.PluginRequest) error {
		if !allowed.Load() {
			return errors.New("coordinator changed")
		}
		return nil
	})
	verifier := &sessionAuthorityTest{epoch: 1, writer: 1}
	registry.SetSessionAuthorizer(func(ctx context.Context, _ string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
		return verifier.AuthorizeNodeSession(ctx, "cluster-1", a, b, action)
	})
	selection := nodeRuntimeFixture(t, s, "")
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	prepared, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Authority: req.Authority, Selection: &selection, CommandID: "runtime", Permission: "read"})
	if err != nil {
		t.Fatal(err)
	}
	req.Plugin = prepared.Runtime
	req.Binding.PluginRuntimeID = prepared.Runtime.ID
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "open"
	opened, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil {
		t.Fatal(err)
	}
	closeReq := nodewire.PluginRequest{Action: nodewire.PluginRuntimeClose, Authority: req.Authority, Runtime: prepared.Runtime, Selection: &prepared.Runtime.Selection}
	allowed.Store(false)
	if _, err := registry.Plugins(t.Context(), "worker", closeReq); err == nil {
		t.Fatal("stale coordinator closed runtime")
	}
	allowed.Store(true)
	if _, err := registry.Plugins(t.Context(), "worker", closeReq); err != nil {
		t.Fatal(err)
	}
	info, err := s.pluginStore().RuntimeInfo(prepared.Runtime.ID)
	if err != nil || len(info.Uses) != 1 || !info.Uses[0].Stopped {
		t.Fatalf("runtime exit not confirmed: %+v %v", info, err)
	}
	req.ID, req.Action = opened.ID, nodewire.SessionActionAttach
	closed, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil || closed.State != nodewire.SessionClosed || !closed.ProcessStopped {
		t.Fatalf("native session not closed: %+v %v", closed, err)
	}
	if _, err := registry.Plugins(t.Context(), "worker", closeReq); err != nil {
		t.Fatalf("lost close response cannot be retried: %v", err)
	}
}

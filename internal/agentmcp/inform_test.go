package agentmcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestContextGuidanceDoesNotRequireAReadBeforeOrdinaryConversation(t *testing.T) {
	server, _ := startServer(t)
	server.SetInformer(&taskInformer{})
	server.Extras("chat", "agent", "token", "")
	list := rpc(t, server.URL(), "token", "tools/list", nil)
	if list.status != http.StatusOK || !strings.Contains(list.rawBody, "steve_context") {
		t.Fatalf("context tool missing from MCP: %+v", list)
	}
	for _, guidance := range []string{Instructions, list.rawBody} {
		if strings.Contains(guidance, "Call steve_context first") || strings.Contains(guidance, "Call it first in a session") {
			t.Fatalf("MCP still requires an unconditional context round trip: %s", guidance)
		}
		if !strings.Contains(guidance, "ordinary questions") || !strings.Contains(guidance, "live state") {
			t.Fatalf("MCP guidance must allow direct replies and require current operational facts: %s", guidance)
		}
	}
}

func TestPlatformOverviewIsAvailableThroughMCP(t *testing.T) {
	server, _ := startServer(t)
	server.SetInformer(&taskInformer{})
	server.Extras("chat", "agent", "token", "")
	out, bad := callTool(t, server.URL(), "token", "steve_help", map[string]any{"topic": "overview"})
	if bad || !strings.Contains(out, "steve_context") || !strings.Contains(out, "steve_projects") || !strings.Contains(out, "steve_delegate") {
		t.Fatalf("platform overview is unavailable via MCP: %s", out)
	}
}

type nodeAddCall struct{ name, address, level, hubURL string }

type capturingFleeter struct {
	Fleeter
	added chan nodeAddCall
}

func (f capturingFleeter) AddNode(_ context.Context, name, address, level, hubURL string) (string, error) {
	f.added <- nodeAddCall{name, address, level, hubURL}
	return "registered", nil
}

func TestNodeAddForwardsAdvertisedHubURLArgument(t *testing.T) {
	server, _ := startServer(t)
	added := make(chan nodeAddCall, 1)
	server.SetFleeter(capturingFleeter{added: added})
	server.SetInformer(&taskInformer{})
	server.Extras("chat", "agent", "token", "")
	want := nodeAddCall{"worker", "127.0.0.1:7701", "restricted", "http://127.0.0.1:7710"}
	out, bad := callTool(t, server.URL(), "token", "steve_node_add", map[string]any{
		"name": want.name, "addr": want.address, "level": want.level, "hub_url": want.hubURL,
	})
	if bad {
		t.Fatal(out)
	}
	select {
	case got := <-added:
		if got != want {
			t.Fatalf("node registration arguments = %+v, want %+v", got, want)
		}
	default:
		t.Fatal("node registration was not called")
	}
}

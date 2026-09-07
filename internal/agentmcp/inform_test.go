package agentmcp

import (
	"context"
	"testing"
)

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

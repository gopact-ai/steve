package admin

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Every node on protocol v2 takes skill bundles, so the skills page never
// marks an up node as unable to sync, whatever its advert lists.
func TestSkillsPageDoesNotMarkAV2NodeUnableToSync(t *testing.T) {
	live, _ := shipperLive(t)
	peer := newSkillPeer(t, "remote")
	a := &Service{LiveSkills: live, Nodes: skillRegistry(t, peer)}
	view, err := a.Skills(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Nodes) != 1 || !view.Nodes[0].Up {
		t.Fatalf("nodes = %+v", view.Nodes)
	}
	raw, _ := json.Marshal(view.Nodes[0])
	if strings.Contains(string(raw), `"takes"`) {
		t.Fatalf("node = %s, want no skill-bundle support flag", raw)
	}
}

// Every node on protocol v2 answers MCP probes and reports its agents' own
// servers, so the MCP page never asks for a node upgrade.
func TestMCPPageDoesNotAskAV2NodeForAnUpgrade(t *testing.T) {
	registry := node.NewRegistry("test-hub", map[string]node.Config{"remote": {DialContext: bareNode(t, "remote")}})
	t.Cleanup(registry.Close)
	a := &Service{ConfigStore: NewConfigStore(&config.Config{}), Nodes: registry}
	view, err := a.MCP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range view.Machines {
		raw, _ := json.Marshal(m)
		if m.Name == "remote" && (!m.Up || strings.Contains(string(raw), `"unsupported"`)) {
			t.Fatalf("machine = %s, want it up with no upgrade flag", raw)
		}
	}
}

// bareNode dials a node that advertises no features and answers every
// config stream with empty settings.
func bareNode(t *testing.T, name string) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if _, err := nodewire.Accept(server, "", nodewire.Advert{Node: name}); err != nil {
				return
			}
			mux := nodewire.NewMux(server, false)
			defer mux.Close()
			for {
				stream, err := mux.Accept(t.Context())
				if err != nil {
					return
				}
				if stream.Request().Kind == nodewire.StreamConfig {
					_ = json.NewEncoder(stream).Encode(nodewire.ConfigReply{})
				}
				_ = stream.Close()
			}
		}()
		t.Cleanup(func() { client.Close() })
		return client, nil
	}
}

package admin

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// Skill bundles are part of protocol v2, so the skills page lists a node
// whose advert names no optional features as up.
func TestSkillsPageListsAV2NodeWithoutFeaturesAsUp(t *testing.T) {
	live, _ := shipperLive(t)
	peer := newSkillPeer(t, "remote")
	a := &Service{LiveSkills: live, Nodes: skillRegistry(t, peer)}
	view, err := a.Skills(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Nodes) != 1 || view.Nodes[0].Name != "remote" || !view.Nodes[0].Up {
		t.Fatalf("nodes = %+v, want remote up", view.Nodes)
	}
}

// MCP probes and a machine's own servers are part of protocol v2, so the
// MCP page lists a node whose advert names no optional features as up.
func TestMCPPageListsAV2NodeWithoutFeaturesAsUp(t *testing.T) {
	registry := node.NewRegistry("test-hub", map[string]node.Config{"remote": {DialContext: bareNode(t, "remote")}})
	t.Cleanup(registry.Close)
	a := &Service{ConfigStore: NewConfigStore(&config.Config{}), Nodes: registry}
	view, err := a.MCP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(view.Machines, func(m consoleapi.MCPMachine) bool { return m.Name == "remote" })
	if i < 0 || !view.Machines[i].Up {
		t.Fatalf("machines = %+v, want remote up", view.Machines)
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

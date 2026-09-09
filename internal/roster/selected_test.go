package roster

import (
	"context"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
)

type selectedNodes struct {
	statuses  []node.Status
	connected [][]string
}

func (n *selectedNodes) Statuses() []node.Status { return n.statuses }
func (n *selectedNodes) EnsureConnected(_ context.Context, names ...string) {
	n.connected = append(n.connected, append([]string(nil), names...))
	for i := range n.statuses {
		if len(names) == 0 || slices.Contains(names, n.statuses[i].Name) {
			n.statuses[i].Up = true
		}
	}
}

func TestForAgentConnectsOnlySelectedDestinationAndKeepsPlacementFacts(t *testing.T) {
	catalog := mustCatalog(t, map[string]agent.Config{"local": {Harness: "mock", Default: true}, "remote": {Node: "worker", Harness: "mock", Requires: []string{"gpu"}}})
	r := New(catalog)
	nodes := &selectedNodes{statuses: []node.Status{
		{Name: "offline"},
		{Name: "worker", Advert: nodewire.Advert{Node: "worker", Capabilities: []string{"gpu"}, Harnesses: []nodewire.Harness{{ID: "mock", Command: "mock", Slots: 3}}}},
	}}
	r.SetNodes(nodes)
	r.SetNodeLevels(map[string]project.Level{"worker": project.LevelRestricted})
	r.SetNodeRegions(map[string]string{"worker": "region-b"})
	r.SetModels(fakeBook{"worker/mock": models.Observation{Current: "small", Available: []string{"small", "large"}}})
	r.SetHubAdvert(func() nodewire.Advert {
		t.Fatal("remote selection inspected unrelated local harnesses")
		return nodewire.Advert{}
	})
	selected, _ := catalog.Resolve("remote")
	c := r.ForAgent(t.Context(), selected)
	if !reflect.DeepEqual(nodes.connected, [][]string{{"worker"}}) {
		t.Fatalf("connected = %v", nodes.connected)
	}
	if nodes.statuses[0].Up {
		t.Fatal("unrelated node was contacted")
	}
	if !c.Up || !c.Eligible || c.Agent.ID != "remote" || c.Node != "worker" || c.Harness != "mock" || c.Slots != 3 || c.Level != project.LevelRestricted || c.Region != "region-b" || c.Model != "small" || len(c.Models) != 2 {
		t.Fatalf("placement facts = %+v", c)
	}
	adm, _, err := r.Admit(t.Context(), c, selected.Requires, nil, "turn")
	if err != nil || !adm.OK() {
		t.Fatalf("selected admission = %+v, %v", adm, err)
	}
	// A newly missing capability must still refuse admission on the same path.
	nodes.statuses[1].Advert.Capabilities = nil
	c = r.ForAgent(t.Context(), selected)
	adm, _, err = r.Admit(t.Context(), c, selected.Requires, nil, "turn-next")
	if err != nil || !adm.Refused() || !strings.Contains(adm.Unmet(), "gpu") {
		t.Fatalf("lost admission refusal = %+v, %v", adm, err)
	}
}

func TestForAgentDialsOnlySelectedRegisteredNode(t *testing.T) {
	catalog := mustCatalog(t, map[string]agent.Config{"remote": {Node: "worker", Harness: "mock", Default: true}})
	r := New(catalog)
	var contacted []string
	dial := func(name string) func(context.Context, string) (net.Conn, error) {
		return func(context.Context, string) (net.Conn, error) {
			contacted = append(contacted, name)
			return nil, errors.New("test node offline")
		}
	}
	registry := node.NewRegistry("test", map[string]node.Config{
		"unrelated": {DialContext: dial("unrelated")},
		"worker":    {DialContext: dial("worker")},
	})
	defer registry.Close()
	r.SetNodes(registry)
	c := r.ForAgent(t.Context(), catalog.Default())
	if !reflect.DeepEqual(contacted, []string{"worker"}) {
		t.Fatalf("contacted = %v", contacted)
	}
	if c.Up || c.Eligible || !strings.Contains(c.Why, "worker is down") {
		t.Fatalf("offline selected node = %+v", c)
	}
}

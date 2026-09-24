package readmodel

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/roster"
)

// dialingNodes reports a fleet with offline machines and spends dial time
// on each of them whenever a caller asks for connections.
type dialingNodes struct {
	statuses []node.Status
	dial     time.Duration
	dials    atomic.Int64
}

func (n *dialingNodes) Statuses() []node.Status { return n.statuses }

func (n *dialingNodes) EnsureConnected(ctx context.Context, names ...string) {
	for _, s := range n.statuses {
		if s.Up {
			continue
		}
		n.dials.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.dial):
		}
	}
}

func offlineFleet(tb testing.TB, dial time.Duration) (*Model, *dialingNodes) {
	tb.Helper()
	configs := map[string]agent.Config{"local": {Harness: "mock", Default: true}}
	nodes := &dialingNodes{dial: dial}
	for i := range 4 {
		name := fmt.Sprintf("node-%d", i)
		status := node.Status{Name: name, Up: i%2 == 0, LastError: "connection refused"}
		if status.Up {
			status.LastError = ""
			status.Advert = nodewire.Advert{Node: name, Harnesses: []nodewire.Harness{{ID: "mock", Command: "mockagent"}}}
		}
		nodes.statuses = append(nodes.statuses, status)
		for j := range 5 {
			configs[fmt.Sprintf("agent-%d-%d", i, j)] = agent.Config{Harness: "mock", Node: name}
		}
	}
	catalog, err := agent.NewCatalog(configs)
	if err != nil {
		tb.Fatal(err)
	}
	r := roster.New(catalog)
	r.SetNodes(nodes)
	return New(Sources{Roster: r, Nodes: nodes}), nodes
}

// BenchmarkSnapshotWithOfflineNodes measures /state's snapshot of a fleet
// whose offline machines take time to dial.
func BenchmarkSnapshotWithOfflineNodes(b *testing.B) {
	m, nodes := offlineFleet(b, time.Millisecond)
	for b.Loop() {
		m.Snapshot(context.Background())
	}
	b.ReportMetric(float64(nodes.dials.Load())/float64(b.N), "dials/op")
}

// /state describes the fleet from what the registry already knows. Dialing
// offline machines belongs to the registry's own redial loop; doing it in
// the request, once for the roster and again for every blocked agent's
// repair, makes the page as slow as the slowest dead node times the
// number of blocked agents.
func TestSnapshotDoesNotDialOfflineNodes(t *testing.T) {
	m, nodes := offlineFleet(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap := m.Snapshot(ctx)
	if n := nodes.dials.Load(); n != 0 {
		t.Fatalf("snapshot dialed offline nodes %d times", n)
	}
	blocked := 0
	for _, a := range snap.Agents {
		if !a.Eligible {
			blocked++
			if a.Reason != roster.ReasonNodeDown {
				t.Fatalf("agent %s blocked for %q, want node down", a.ID, a.Reason)
			}
		}
	}
	if blocked != 10 {
		t.Fatalf("%d agents blocked, want the 10 on offline nodes", blocked)
	}
}

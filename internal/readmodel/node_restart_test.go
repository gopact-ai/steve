package readmodel

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node"
)

// The fleet shows each machine's latest restart, by hand or automatic, as
// it was recorded among the events: the one that happened last, whichever
// was recorded last. A machine never restarted shows none.
func TestFleetShowsEachMachinesLatestRestart(t *testing.T) {
	nodes := fakeNodes{statuses: []node.Status{{Name: "node-dev", Up: true}, {Name: "node-other", Up: true}}}
	model := New(Sources{Hub: Hub{Node: "hub-1"}, Nodes: nodes})
	at := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	model.ObserveAt(at, "node.restart", "node-dev", "node-dev: automatic restart by hub-1 failed", map[string]string{"by": "hub-1", "trigger": "automatic", "outcome": "failed", "reason": "The peer did not stay running"})
	model.ObserveAt(at.Add(2*time.Minute), "node.restart", "node-dev", "node-dev: restart by hub-1 restarted", map[string]string{"by": "hub-1", "trigger": "manual", "outcome": "restarted"})
	model.ObserveAt(at.Add(3*time.Minute), "node.down", "node-dev", "node-dev down", map[string]string{"reason": "disconnected"})
	model.ObserveAt(at.Add(time.Minute), "node.restart", "node-dev", "node-dev: automatic restart by hub-1 started", map[string]string{"by": "hub-1", "trigger": "automatic", "outcome": "started"})
	listed := map[string]Node{}
	for _, n := range model.Snapshot(t.Context()).Nodes {
		listed[n.Name] = n
	}
	want := NodeRestart{At: at.Add(2 * time.Minute), By: "hub-1", Trigger: "manual", Outcome: "restarted"}
	if got := listed["node-dev"].LastRestart; got == nil || *got != want {
		t.Fatalf("node-dev last restart = %+v, want %+v", got, want)
	}
	if got := listed["node-other"].LastRestart; got != nil {
		t.Fatalf("a machine never restarted shows a restart: %+v", got)
	}
}

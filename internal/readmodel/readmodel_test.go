package readmodel

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

type fakeNodes struct{ statuses []node.Status }

func (f fakeNodes) Statuses() []node.Status { return f.statuses }

func (fakeNodes) EnsureConnected(context.Context) {}

func fixture(t *testing.T) *Model {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":   {Harness: "mock", Default: true},
		"builder": {Harness: "mock", Node: "node-a", Requires: []string{"gpu"}},
		// A harness whose binary is missing on node-a, with builder there
		// to fix it: the snapshot should say so.
		"fixme": {Harness: "absent", Node: "node-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := roster.New(catalog)
	r.SetHubCapabilities([]string{"basic"})
	nodes := fakeNodes{statuses: []node.Status{
		{Name: "node-a", Addr: "10.0.0.1:7701", Up: true, Advert: nodewire.Advert{
			Node: "node-a", OS: "linux", Arch: "amd64", Capabilities: []string{"gpu"},
			Harnesses: []nodewire.Harness{
				{ID: "mock", Command: "mockagent", Models: []string{"m1"}},
				{ID: "absent", Command: "absent-bin", Missing: `"absent-bin" not on this node's PATH`},
			},
		}},
		{Name: "node-b", Addr: "10.0.0.2:7701", Up: false, LastError: "connection refused"},
	}}
	r.SetNodes(nodes)

	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasks.SetBudget(24, time.Hour) // a budget is opt-in; the snapshot must still carry one when set
	parent, err := tasks.Create(task.Task{Goal: "ship it", Channel: "chat", Member: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{
		Goal: "build it", Channel: "chat", Member: "builder", Node: "node-a", Parent: parent.ID,
	}); err != nil {
		t.Fatal(err)
	}

	plans, err := plan.Open(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Create(plan.Plan{
		TaskID: parent.ID, Goal: "ship it", By: "rule",
		Steps: []plan.Step{{
			ID: "build", Goal: "compile", Requires: []string{"gpu"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// What harnesses were seen running: the hub's mock, by a session.
	seen := models.New()
	seen.Observe(models.Observation{Harness: "mock", Current: "mock-fast", Available: []string{"mock-fast", "mock-deep"}, Source: "session"})
	r.SetModels(seen)

	return New(Sources{
		Hub: Hub{Node: "hub-1", Capabilities: []string{"basic"}},
		HubAdvert: func() nodewire.Advert {
			return nodewire.Advert{Node: "hub-1", Hostname: "hub-1.local", Harnesses: []nodewire.Harness{{ID: "mock", Command: "mockagent"}}}
		},
		Models: seen,
		Roster: r, Nodes: nodes, Tasks: tasks, Plans: plans,
	})
}

func TestSnapshotCoversTheWholeSystem(t *testing.T) {
	snap := fixture(t).Snapshot(t.Context())

	// The hub is a node too — first, with the coordinating role — and the
	// workers follow, the dead one included.
	if len(snap.Nodes) != 3 || snap.Nodes[0].Role != RoleHub || snap.Nodes[0].Name != snap.Hub.Node || !snap.Nodes[0].Up {
		t.Fatalf("nodes = %+v, want the hub first then both workers", snap.Nodes)
	}
	for _, n := range snap.Nodes[1:] {
		if n.Role != RoleWorker {
			t.Fatalf("worker %s has role %q", n.Name, n.Role)
		}
	}
	// An agent on the hub is placed on a named machine, not on a role.
	for _, a := range snap.Agents {
		if a.Node == "" {
			t.Fatalf("agent %s has no place", a.ID)
		}
	}
	// A node that is down must still appear, with its reason: a roster that
	// hides what is broken sends someone hunting for a ghost.
	var down Node
	for _, n := range snap.Nodes {
		if n.Name == "node-b" {
			down = n
		}
	}
	if down.Up || down.LastError == "" {
		t.Fatalf("down node = %+v, want it listed with a reason", down)
	}
	if len(snap.Agents) != 3 {
		t.Fatalf("agents = %d", len(snap.Agents))
	}
	if len(snap.Plans) != 1 || len(snap.Plans[0].Steps) != 1 {
		t.Fatalf("plans = %+v", snap.Plans)
	}

	// The tree must be linked, because delegated work is the case a flat
	// list hides.
	var parent Task
	for _, item := range snap.Tasks {
		if item.Parent == "" {
			parent = item
		}
	}
	if len(parent.Children) != 1 {
		t.Fatalf("parent task children = %v, want the delegated task", parent.Children)
	}
	if parent.MaxTurns == 0 || parent.MaxElapse == "" {
		t.Fatalf("budget missing from the snapshot: %+v", parent)
	}
}

// The stream carries gopact's node transitions straight through.
func TestEventStreamCarriesWorkflowTransitions(t *testing.T) {
	model := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, stop := model.Subscribe(ctx)
	defer stop()

	if err := model.Emit(ctx, gopact.Event{
		Type: "node.started", NodeID: "build", RunID: "run-1", Sequence: 4,
		Timestamp: time.Now(), Summary: "builder on node-a",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-stream:
		if ev.Kind != "node.started" || ev.StepID != "build" || ev.Seq != 4 || ev.RunID != "run-1" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
	}
}

// A slow renderer must never stall the workflow runtime.
func TestSlowSubscriberDoesNotBlockThePublisher(t *testing.T) {
	model := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, stop := model.Subscribe(ctx); stop != nil {
		defer stop()
	}
	done := make(chan struct{})
	go func() {
		for i := range 500 {
			_ = model.Emit(ctx, gopact.Event{Type: "node.completed", Sequence: int64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publishing blocked on a subscriber that never read")
	}
}

// The model column comes from observation: a harness seen running reports
// its model on the agent and on the node, and a blocked agent whose
// binary is missing names who could repair it.
func TestSnapshotShowsObservedModelsAndRepairs(t *testing.T) {
	snap := fixture(t).Snapshot(t.Context())
	byID := map[string]Agent{}
	for _, a := range snap.Agents {
		byID[a.ID] = a
	}
	if a := byID["local"]; a.Model != "mock-fast" || len(a.Models) != 2 {
		t.Fatalf("local = %+v, want the observed model and both alternatives", a)
	}
	if a := byID["builder"]; a.Model != "" || len(a.Models) != 1 || a.Models[0] != "m1" {
		t.Fatalf("builder = %+v, want the node's declared model only", a)
	}
	if a := byID["fixme"]; a.Eligible || a.Repair != "builder" || a.Node != "node-a" {
		t.Fatalf("fixme = %+v, want blocked with builder as the repair", a)
	}
	// The hub's node row carries what was observed on its harness, and
	// the fresh advert's identity.
	hub := snap.Nodes[0]
	if hub.Role != RoleHub || hub.Host != "hub-1.local" || len(hub.Harnesses) != 1 || hub.Harnesses[0].Model != "mock-fast" {
		t.Fatalf("hub row = %+v", hub)
	}
}

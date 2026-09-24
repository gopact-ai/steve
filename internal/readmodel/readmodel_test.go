package readmodel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
)

type fakeNodes struct{ statuses []node.Status }

func (f fakeNodes) Statuses() []node.Status { return f.statuses }

func (fakeNodes) EnsureConnected(context.Context, ...string) {}

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

	tasks, err := task.OpenLedger(testLedger(t))
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

	plans, err := plan.OpenLedger(testLedger(t))
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

// A subscriber that misses an event is told so by the end of its stream:
// a reader that stays connected must be able to trust it saw everything.
func TestSubscriberThatFallsBehindIsClosed(t *testing.T) {
	model := New(Sources{})
	slow, stopSlow := model.Subscribe(t.Context())
	defer stopSlow()
	keeping, stopKeeping := model.Subscribe(t.Context())
	defer stopKeeping()

	var kept int
	for i := range 200 {
		model.Publish(Event{Kind: "task.changed", Seq: int64(i)})
		// The reader that keeps up drains as it goes.
		for drained := false; !drained; {
			select {
			case <-keeping:
				kept++
			default:
				drained = true
			}
		}
	}
	if kept != 200 {
		t.Fatalf("a reader that keeps up saw %d of 200 events", kept)
	}
	received := 0
	for ended := false; !ended; {
		select {
		case _, open := <-slow:
			if !open {
				ended = true
			} else {
				received++
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the slow reader's stream stayed open after %d events", received)
		}
	}
	if received == 0 || received >= 200 {
		t.Fatalf("the slow reader received %d events before its stream ended, want its buffer and then the end", received)
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

func TestSnapshotCarriesMemberDisplayNames(t *testing.T) {
	m := fixture(t)
	m.src.NodeNames = func() map[string]string { return map[string]string{"hub-1": "我的 Mac", "node-a": "GPU 工作站"} }
	snap := m.Snapshot(t.Context())
	got := map[string]string{}
	for _, n := range snap.Nodes {
		got[n.Name] = n.DisplayName
	}
	want := map[string]string{"hub-1": "我的 Mac", "node-a": "GPU 工作站", "node-b": ""}
	for name, display := range want {
		if got[name] != display {
			t.Fatalf("display name of %s = %q, want %q (all: %v)", name, got[name], display, got)
		}
	}
	if snap.Nodes[0].Name != "hub-1" {
		t.Fatalf("names stay the identity; hub row = %+v", snap.Nodes[0])
	}
}

// A machine the hub refused for its protocol version says so, with the
// versions that met and which side has to be upgraded, so a reader can tell
// it from a machine that is merely down; the down machine carries no such
// field.
func TestSnapshotSaysWhichMachineWasRefusedForItsProtocol(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"local": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	r := roster.New(catalog)
	reason := "hub speaks v2–v2, node speaks v1–v1"
	ahead := "hub speaks v2–v2, node speaks v3–v3"
	nodes := fakeNodes{statuses: []node.Status{
		{Name: "old", LastError: "nodewire: protocol version mismatch: " + reason, Mismatch: &nodewire.VersionMismatch{Node: 1, HubMin: 2, HubMax: 2, Reason: reason}},
		{Name: "newer", LastError: "nodewire: protocol version mismatch: " + ahead, Mismatch: &nodewire.VersionMismatch{Node: 3, HubMin: 2, HubMax: 2, Reason: ahead}},
		{Name: "gone", LastError: "connection refused"},
	}}
	r.SetNodes(nodes)
	snap := New(Sources{Hub: Hub{Node: "hub-1"}, Roster: r, Nodes: nodes}).Snapshot(t.Context())
	listed := map[string]Node{}
	for _, n := range snap.Nodes {
		listed[n.Name] = n
	}
	old, newer, gone := listed["old"], listed["newer"], listed["gone"]
	if old.ProtocolMismatch == nil || *old.ProtocolMismatch != (ProtocolMismatch{Node: 1, HubMin: 2, HubMax: 2, Upgrade: UpgradeNode}) {
		t.Fatalf("refused machine = %+v, want its protocol mismatch and the machine to upgrade", old)
	}
	raw, _ := json.Marshal(old)
	if !strings.Contains(string(raw), `"protocol_mismatch":{"node":1,"hub_min":2,"hub_max":2,"upgrade":"node"}`) {
		t.Fatalf("refused machine = %s, want protocol_mismatch", raw)
	}
	if newer.ProtocolMismatch == nil || *newer.ProtocolMismatch != (ProtocolMismatch{Node: 3, HubMin: 2, HubMax: 2, Upgrade: UpgradeHub}) {
		t.Fatalf("refused machine = %+v, want its protocol mismatch and the hub to upgrade", newer)
	}
	raw, _ = json.Marshal(newer)
	if !strings.Contains(string(raw), `"protocol_mismatch":{"node":3,"hub_min":2,"hub_max":2,"upgrade":"hub"}`) {
		t.Fatalf("refused machine = %s, want protocol_mismatch", raw)
	}
	raw, _ = json.Marshal(gone)
	if gone.Name != "gone" || strings.Contains(string(raw), "protocol_mismatch") {
		t.Fatalf("down machine = %s, want no protocol_mismatch", raw)
	}
}

// testLedger opens a ledger that lives as long as the test.
func testLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}

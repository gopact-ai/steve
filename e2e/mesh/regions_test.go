package mesh

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
)

// C10: node-b belongs to region "west", whose leases are issued by another
// hub — played here by a second ledger served over HTTP. A step placed on
// node-b takes its workspace and slot leases from west; the east hub's
// attempt record carries them, its transitions are fenced through west's
// check, and cutting the lease in west stops the attempt.
func TestC10RegionalLeasesAreIssuedByTheNodesRegion(t *testing.T) {
	requireMesh(t)
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{
		nodeA: {Addr: addrA(), Token: tokenA(), DialTimeout: 10 * time.Second},
		nodeB: {Addr: addrB(), Token: tokenB(), DialTimeout: 10 * time.Second, Region: "west"},
	})
	t.Cleanup(reg.Close)
	reg.EnsureConnected(t.Context())

	f := newFleet(t)
	f.manager.SetTransports(reg)
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":   {Harness: "mock", Default: true},
		"shipper": {Harness: "mock", Node: nodeB, Requires: []string{"internal-net"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fleetRoster := roster.New(catalog)
	fleetRoster.SetNodes(reg)
	fleetRoster.SetHubCapabilities([]string{"basic"})
	fleetRoster.SetNodeRegions(map[string]string{nodeB: "west"})

	dir := t.TempDir()
	_, attempts, artifacts := declareProjects(t, dir, reg)
	// The east hub's ledger is the one declareProjects opened; west is a
	// second hub for this test.
	west, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { west.Close() })
	west.SetRegion("west")
	server := httptest.NewServer(ledger.IssuerHandler(west, "west-token"))
	t.Cleanup(server.Close)
	east := ledgerOf(t, dir)
	east.SetRegion("east")
	east.RegisterIssuer("west", ledger.NewHTTPIssuer(server.URL, "west-token"))

	deps := exec.Deps{Workspaces: artifacts, Attempts: attempts, Artifacts: artifacts, Roster: fleetRoster,
		Runner: exec.NewAgentRunner(f.manager, noCaps{}, fleetRoster), Recorder: f.plans}
	runs := exec.NewRuns(workflow.NewMemoryStore())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	created, err := f.plans.Create(plan.Plan{ProjectID: "local", TaskID: "e2e-region", Goal: "regions", By: "declared",
		Steps: []plan.Step{{ID: "west-work", Goal: "say west", Requires: []string{"internal-net"}, State: plan.StepPending,
			Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "e2e"}}}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runs.Execute(ctx, created, deps)
	if err != nil || outcome.Err != nil {
		t.Fatalf("plan on the west node failed: %v %v", err, outcome.Err)
	}
	records, _ := attempts.ForTask(ctx, "e2e-region")
	if len(records) != 1 || records[0].State != "bound" || records[0].Node != nodeB {
		t.Fatalf("attempts = %+v", records)
	}
	var workspaceLease string
	for _, l := range records[0].Leases {
		if strings.HasPrefix(l.Key, "workspace:") {
			workspaceLease = l.Key
			if l.Region != "west" {
				t.Fatalf("workspace lease region = %q, want west", l.Region)
			}
		}
	}
	if workspaceLease == "" {
		t.Fatalf("no workspace lease on the attempt: %+v", records[0].Leases)
	}
	// It was issued in west and released there when the attempt bound.
	if l, ok, _ := west.LeaseOf(ctx, workspaceLease); !ok || l.Holder != "" {
		t.Fatalf("west's copy of %s = %+v ok=%v", workspaceLease, l, ok)
	}
	if _, ok, _ := east.LeaseOf(ctx, workspaceLease); ok {
		t.Fatal("the west lease was written into east's ledger")
	}
	// The bound event names the west lease among its fencings.
	events, _ := attempts.History(ctx, records[0].ID)
	last := events[len(events)-1]
	fenced := false
	for _, l := range last.Fencings {
		if l.Key == workspaceLease && l.Region == "west" {
			fenced = true
		}
	}
	if !fenced {
		t.Fatalf("the bound transition was not fenced on the west lease: %+v", last.Fencings)
	}
}

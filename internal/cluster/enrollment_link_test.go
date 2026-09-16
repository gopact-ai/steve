package cluster

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect/linktest"
	"github.com/hashicorp/raft"
)

// A machine enrolled over SSH never has to reach this node on the network:
// the hub advertises an address nobody can route to, and the enrollment
// still completes because the two talk through the session's forwards.
// The machine's package tells its node to reach the hub at the tunnel
// ports rather than at what the hub advertises; the node starts before the
// link is up, as it does on a real machine, and joins once it is. The link
// is recorded so that a restart of the hub reopens it.
func TestEnrollmentOverSSHCarriesTheClusterProtocolThroughTheSession(t *testing.T) {
	tunnels := &linktest.Launcher{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	hubConfig, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	hubConfig.RaftAddress = "only-a-tunnel-reaches-it.invalid:0"
	hubConfig.PeerAddress = "only-a-tunnel-reaches-it.invalid:0"
	hubConfig.PeerURL = "https://only-a-tunnel-reaches-it.invalid:0"
	if err := SaveClusterJSON(hubOptions.ClusterPath, hubConfig, false); err != nil {
		t.Fatal(err)
	}
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)

	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	hubRoute := coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: hubRoute, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatalf("a routed enrollment was refused: %v", err)
	}
	if !strings.Contains(strings.Join(plan.Effects, "\n"), hubRaftOnMachine) {
		t.Fatalf("the plan does not tell the user about the tunnel: %v", plan.Effects)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	const id = "over-ssh"
	prepared, err := hub.PrepareEnrollment(t.Context(), request, id, true)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportPeerPackage(prepared.Payload, ClusterPeerTestDir(t)+"/box")
	if err != nil {
		t.Fatal(err)
	}
	nodeConfig, err := LoadClusterPeerConfig(imported.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if nodeConfig.Routes[hub.Config.NodeID] != hubRoute {
		t.Fatalf("the machine was not told to reach the hub through the tunnel: %+v", nodeConfig.Routes)
	}

	nodeOptions := PeerOptions{ConfigPath: imported.ConfigPath, ClusterPath: imported.ClusterPath, RaftConfig: raft.DefaultConfig(), PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-domain-box", nil }, Activate: testPeerApplication(t, &activations)}
	node := StartTestPeer(t, nodeOptions)
	linkCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(linkCtx, id); err != nil {
		t.Fatalf("the link did not come up: %v", err)
	}
	route, ok := hub.routes.Lookup(prepared.NodeID)
	if !ok || !strings.HasPrefix(route.Raft, "127.0.0.1:") || !strings.HasPrefix(route.API, "127.0.0.1:") {
		t.Fatalf("the hub has no route to the machine through the link: %+v %v", route, ok)
	}
	saved, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if link := saved.Links[prepared.NodeID]; link.Alias != "box" || link.Remote != hubRoute || !strings.HasSuffix(link.Peer.API, ":"+peerAddress[strings.LastIndex(peerAddress, ":")+1:]) {
		t.Fatalf("the link was not recorded for the next start: %+v", saved.Links)
	}
	// The same enrollment asking again keeps the session it has.
	if err := hub.OpenEnrollmentLink(linkCtx, id); err != nil || tunnels.Count() != 1 {
		t.Fatalf("a second request replaced a working link: %v (%d sessions)", err, tunnels.Count())
	}

	// The enrollment goes from waiting for the node, through the join and
	// its mesh check, to registering the worker; that last step needs the
	// real application, which this fixture does not run. Everything before
	// it is the protocol crossing the tunnel in both directions.
	deadline := time.Now().Add(30 * time.Second)
	var result PeerEnrollmentResult
	for {
		result, err = hub.CompletePeerEnrollment(t.Context(), id)
		if result.Phase == "registering_worker" || err == nil && result.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node did not join through the tunnel: phase=%s error=%v", result.Phase, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	state, err := hub.Runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Voters[node.Config.NodeID] == "" || result.NodeID != node.Config.NodeID {
		t.Fatalf("the machine is not a voter after joining through the tunnel: %+v", state.Voters)
	}
	if _, ok := hub.LinkStatuses()[node.Config.NodeID]; !ok {
		t.Fatal("the link to a joined machine was dropped")
	}
	// Removed from the page, the machine leaves the cluster and its link
	// and route go with it.
	if err := hub.RemoveMember(t.Context(), node.Config.NodeID); err != nil {
		t.Fatal(err)
	}
	state, err = hub.Runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, member := state.Members[node.Config.NodeID]; member {
		t.Fatal("the removed machine is still a member")
	}
	if _, ok := hub.LinkStatuses()[node.Config.NodeID]; ok {
		t.Fatal("the link to a removed machine remains")
	}
	if _, ok := hub.routes.Lookup(node.Config.NodeID); ok {
		t.Fatal("the route to a removed machine remains")
	}
}

// Giving up an enrollment closes its session and forgets the link and the
// route, so nothing keeps dialing a machine that is not coming.
func TestAbandoningAnEnrollmentDropsItsLink(t *testing.T) {
	tunnels := &linktest.Launcher{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)
	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := hub.PrepareEnrollment(t.Context(), request, "given-up", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(ctx, "given-up"); err != nil {
		t.Fatal(err)
	}
	if err := hub.AbandonPeerEnrollment(t.Context(), "given-up"); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.routes.Lookup(prepared.NodeID); ok {
		t.Fatal("the route to an abandoned machine remains")
	}
	if _, ok := hub.LinkStatuses()[prepared.NodeID]; ok {
		t.Fatal("the link to an abandoned machine remains")
	}
	saved, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Links[prepared.NodeID]; ok {
		t.Fatal("the abandoned link would be reopened at the next start")
	}
	select {
	case <-tunnels.Ended(0):
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned session was not ended")
	}
}

// A hub that restarts reopens the session to every machine it enrolled
// and routes to it at the new session's ports; when it stops, the session
// is ended only after the runtime has said its goodbyes through it.
func TestARestartedHubReopensItsLinksAndClosesThemAfterTheRuntime(t *testing.T) {
	tunnels := &linktest.Launcher{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub, err := OpenPeer(context.Background(), hubOptions)
	if err != nil {
		t.Fatal(err)
	}
	WaitPeerReady(t, hub)
	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := hub.PrepareEnrollment(t.Context(), request, "survives-restart", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(ctx, "survives-restart"); err != nil {
		t.Fatal(err)
	}
	var runtimeClosedFirst atomic.Bool
	runtime := hub.Runtime.Load()
	tunnels.SetOnEnd(func() {
		select {
		case <-runtime.closeDone:
			runtimeClosedFirst.Store(true)
		default:
		}
	})
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tunnels.Ended(0):
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the hub did not end its session")
	}
	if !runtimeClosedFirst.Load() {
		t.Fatal("the session was ended before the runtime closed; its last messages had no way through")
	}

	tunnels.SetOnEnd(nil)
	restarted := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, restarted)
	restarted.Mu.RLock()
	link := restarted.links[prepared.NodeID]
	restarted.Mu.RUnlock()
	if link == nil {
		t.Fatal("the restarted hub did not reopen the link")
	}
	if err := link.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	route, ok := restarted.routes.Lookup(prepared.NodeID)
	if !ok || tunnels.Count() != 2 {
		t.Fatalf("the restarted hub has no route through a new session: %+v %v (%d sessions)", route, ok, tunnels.Count())
	}
	outbound := link.Status().Outbound
	if len(outbound) != 2 || route.Raft != outbound[0].Listen || route.API != outbound[1].Listen {
		t.Fatalf("the route %+v does not point at the reopened link's listeners %v", route, outbound)
	}
}

// Removing a machine from the page takes it out of the cluster and ends
// the session and route this node kept for it; a machine that was never a
// member is only cleaned up, and this node cannot remove itself.
func TestRemovingAMemberEndsItsLinkAndRoute(t *testing.T) {
	tunnels := &linktest.Launcher{}
	hubOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	hubOptions.Activate = testPeerApplication(t, &activations)
	hubOptions.SSHLaunch = tunnels
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)
	if err := hub.RemoveMember(t.Context(), hub.Config.NodeID); err == nil {
		t.Fatal("this node removed itself")
	}
	peerAddress, raftAddress := FreeEnrollmentPorts(t)
	hubRaftOnMachine, hubAPIOnMachine := FreeEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Alias: "box", Name: "box", PeerAddress: peerAddress, RaftAddress: raftAddress, HubRoute: coordination.Route{Raft: hubRaftOnMachine, API: hubAPIOnMachine}, Level: "restricted"}
	plan, err := hub.PreviewEnrollment(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	request = plan.Request
	request.ExpectedPlanHash = plan.ReviewID
	prepared, err := hub.PrepareEnrollment(t.Context(), request, "to-remove", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := hub.OpenEnrollmentLink(ctx, "to-remove"); err != nil {
		t.Fatal(err)
	}
	// Not yet a member: only the link and route are dropped.
	if err := hub.RemoveMember(t.Context(), prepared.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.routes.Lookup(prepared.NodeID); ok {
		t.Fatal("the route to a removed machine remains")
	}
	if _, ok := hub.LinkStatuses()[prepared.NodeID]; ok {
		t.Fatal("the link to a removed machine remains")
	}
	saved, err := LoadClusterPeerConfig(hubOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Links[prepared.NodeID]; ok {
		t.Fatal("the removed link would be reopened at the next start")
	}
	select {
	case <-tunnels.Ended(0):
	case <-time.After(2 * time.Second):
		t.Fatal("the removed machine's session was not ended")
	}
}

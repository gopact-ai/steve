package cluster

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

// The hub of a personal fleet is a laptop whose address changes with the
// network it is on, and a machine behind an SSH tunnel reaches it only at a
// loopback port of its own. The hub advertises an address nobody can route
// to; the joining node still gets in because its route to the hub is what
// it dials, and the hub's own checks of the node's independent connection
// pass the same way.
func TestANodeJoinsThroughItsRouteWhenTheHubAdvertisesAnAddressItCannotReach(t *testing.T) {
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
	hub := StartTestPeer(t, hubOptions)
	WaitPeerReady(t, hub)
	if !strings.HasPrefix(hub.Config.PeerURL, "https://only-a-tunnel-reaches-it.invalid:") {
		t.Fatalf("fixture: hub advertises %s", hub.Config.PeerURL)
	}

	nodeOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), hub)
	nodeConfig, err := LoadClusterPeerConfig(nodeOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if nodeConfig.Seeds[0].APIAddress != hub.Config.PeerURL {
		t.Fatalf("fixture: seeds carry %s", nodeConfig.Seeds[0].APIAddress)
	}
	nodeConfig.Routes = map[string]coordination.Route{hub.Config.NodeID: {Raft: hub.Config.RaftBindAddress, API: hub.Config.PeerBindAddress}}
	if err := SaveClusterJSON(nodeOptions.ClusterPath, nodeConfig, false); err != nil {
		t.Fatal(err)
	}
	nodeOptions.Activate = testPeerApplication(t, &activations)
	node := StartTestPeer(t, nodeOptions)
	member := coordination.Member{NodeID: node.Config.NodeID, Name: node.Config.Name, Address: node.Config.RaftAddress, APIAddress: node.Config.PeerURL}
	if _, err := hub.Join(t.Context(), coordination.JoinRequest{ID: "join-through-route", Actor: "owner", Member: member}); err != nil {
		t.Fatalf("the node did not join through its route: %v", err)
	}
	// A write made at the node goes to the hub, which leads; the node can
	// only get there through its route.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := PeerRequest(t, node, http.MethodPost, "/console/test", map[string]string{"message": "via-route"})
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a write from the node did not reach the hub through the route: %d %s", status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	status, body := PeerRequest(t, hub, http.MethodGet, "/console/test", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "via-route") {
		t.Fatalf("the hub did not receive the node's write: %d %s", status, body)
	}
	hubURL, _ := url.Parse(hub.Config.PeerURL)
	if hubURL.Hostname() != "only-a-tunnel-reaches-it.invalid" {
		t.Fatalf("the hub's advertised address changed: %s", hub.Config.PeerURL)
	}
}

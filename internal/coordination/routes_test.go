package coordination

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// A machine reached through an SSH tunnel answers at a loopback port that
// exists only on this node. The other node still advertises whatever address
// it has; this node's route to it is what its calls must follow.
func TestClientFollowsThisNodesRouteToAPeerInsteadOfTheAdvertisedAddress(t *testing.T) {
	c := newTLSTestCluster(t, 2)
	real, err := url.Parse(c.members["node-2"].APIAddress)
	if err != nil {
		t.Fatal(err)
	}
	unroutable := "https://" + net.JoinHostPort("only-a-tunnel-reaches-it.invalid", real.Port())
	routes := NewRouteTable(map[string]Route{"node-2": {API: real.Host}})
	client, err := NewClient(ClientConfig{TLS: c.identities["node-1"], Routes: routes, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	progress, err := client.Probe(ctx, Member{NodeID: "node-2", APIAddress: unroutable})
	if err != nil || progress.NodeID != "node-2" {
		t.Fatalf("the route was not followed: %+v %v", progress, err)
	}
	routes.Delete("node-2")
	if _, err := client.Probe(ctx, Member{NodeID: "node-2", APIAddress: unroutable}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("without a route the advertised address is dialed: %v", err)
	}
	// The route changed, so the pooled client was replaced, not kept beside a
	// new one: one client per node, whatever the route history.
	client.mu.Lock()
	kept := len(client.clients)
	client.mu.Unlock()
	if kept != 1 {
		t.Fatalf("expected one pooled client for node-2, kept %d", kept)
	}
}

func TestRaftDialsAPeerThroughThisNodesRoute(t *testing.T) {
	c := newTLSTestCluster(t, 2)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	routes := NewRouteTable(map[string]Route{"node-2": {Raft: c.members["node-2"].Address}})
	stream, err := NewTLSStreamLayer(listener, c.identities["node-1"], func(raft.ServerAddress) string { return "node-2" }, routes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	connection, err := stream.Dial("only-a-tunnel-reaches-it.invalid:1", 2*time.Second)
	if err != nil {
		t.Fatalf("raft did not follow the route: %v", err)
	}
	connection.Close()
	routes.Delete("node-2")
	if _, err := stream.Dial("only-a-tunnel-reaches-it.invalid:1", 2*time.Second); err == nil {
		t.Fatal("without a route the advertised address is dialed")
	}
}

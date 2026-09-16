package coordination

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"
)

// A laptop on a VPN often cannot connect to its own tunnel address even
// though every other machine reaches it there. Its own node is still the
// same process, so checks aimed at itself must go over loopback while
// every other node keeps being dialed at the address it advertises.
func TestClientReachesItsOwnNodeOverLoopbackWhenTheAdvertisedHostIsNotSelfRoutable(t *testing.T) {
	c := newTLSTestCluster(t, 1)
	client := c.clients["node-1"]
	self := c.members["node-1"]
	endpoint, err := url.Parse(self.APIAddress)
	if err != nil {
		t.Fatal(err)
	}
	unroutable := "https://" + net.JoinHostPort("self-only-others-can-route.invalid", endpoint.Port())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	progress, err := client.Probe(ctx, Member{NodeID: "node-1", APIAddress: unroutable})
	if err != nil || progress.NodeID != "node-1" {
		t.Fatalf("own node was not probed over loopback: %+v %v", progress, err)
	}
	_, err = client.Probe(ctx, Member{NodeID: "node-2", APIAddress: unroutable})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("another node was not dialed at its advertised address: %v", err)
	}
}

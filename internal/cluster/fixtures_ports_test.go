package cluster

import (
	"net"
	"testing"
)

// The addresses an enrollment test plans with stay bound until the test
// ends or hands them to what serves them. A port released early can be
// taken by any other process before its owner binds it.
func TestEnrollmentPortsStayBoundUntilHandedOver(t *testing.T) {
	peerAddress, raftAddress := HoldEnrollmentPorts(t).Addresses()
	for _, address := range []string{peerAddress, raftAddress} {
		if taken, err := net.Listen("tcp", address); err == nil {
			taken.Close()
			t.Fatalf("%s was free for anyone to bind before its owner took it", address)
		}
	}
}

// Handed-over listeners are served only where the configuration says the
// node listens; one bound elsewhere is refused rather than advertised.
func TestHandedOverListenersMustBeWhereTheConfigurationSays(t *testing.T) {
	held := HoldEnrollmentPorts(t)
	peerAddress, raftAddress := held.Addresses()
	raft, peer, err := bindPeerListeners(PeerConfig{RaftBindAddress: raftAddress, PeerBindAddress: peerAddress}, held)
	if err != nil || raft != held.Raft || peer != held.Peer {
		t.Fatalf("the held listeners were not served: %v", err)
	}
	if _, _, err := bindPeerListeners(PeerConfig{RaftBindAddress: peerAddress, PeerBindAddress: raftAddress}, held); err == nil {
		t.Fatal("listeners at other ports than configured were accepted")
	}
	if _, _, err := bindPeerListeners(PeerConfig{RaftBindAddress: "127.0.0.1:0", PeerBindAddress: "127.0.0.1:0"}, held); err != nil {
		t.Fatalf("a configuration that leaves the ports to the system refused them: %v", err)
	}
}

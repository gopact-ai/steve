package coordination

import "testing"

func TestTransportPeersExposeRaftMembershipIndependentlyOfFSMProjection(t *testing.T) {
	cluster := newTestCluster(t, 3)
	node := cluster.leader()
	peers := node.TransportPeers()
	if len(peers) != 3 {
		t.Fatalf("transport peers do not contain current voters: %+v", peers)
	}
	for id, member := range node.Status().Members {
		if peers[id] != member.Address {
			t.Fatalf("transport address differs for %s: %s", id, peers[id])
		}
	}
	delete(peers, "node-2")
	if len(node.TransportPeers()) != 3 {
		t.Fatal("caller changed the retained Raft membership")
	}
}

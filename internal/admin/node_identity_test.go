package admin

import (
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

// Two services in one process administer two machines: each answers for
// the node it was given.
func TestServiceAnswersForItsOwnNode(t *testing.T) {
	a := &Service{NodeName: "node-a", ClusterMode: true}
	b := &Service{NodeName: "node-b", ClusterMode: true}
	for _, s := range []*Service{a, b} {
		if got := s.nodeKey("hub"); got != s.NodeName {
			t.Fatalf("%s: nodeKey(hub) = %q", s.NodeName, got)
		}
		if !s.localMachine(s.NodeName) {
			t.Fatalf("%s: its own node is not local", s.NodeName)
		}
		if err := s.RemoveNode(t.Context(), s.NodeName); err == nil {
			t.Fatalf("%s: removed itself", s.NodeName)
		}
	}
	if a.localMachine(b.NodeName) || b.localMachine(a.NodeName) {
		t.Fatal("a service took another service's node for its own")
	}
}

func TestObservedHubAdvertNamesTheGivenNode(t *testing.T) {
	adv := ObservedHubAdvert("node-a", NewConfigStore(&config.Config{}), &LocalObservation{})
	if adv.Node != "node-a" || adv.Snapshot == nil || adv.Snapshot.Node != "node-a" {
		t.Fatalf("advert names %q, snapshot %+v", adv.Node, adv.Snapshot)
	}
}

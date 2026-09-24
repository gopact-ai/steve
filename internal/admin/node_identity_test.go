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

// Each observation numbers the hub snapshots it describes: a second
// application in the same process starts its own generation and sequence.
func TestHubSnapshotsAreNumberedPerObservation(t *testing.T) {
	store := NewConfigStore(&config.Config{})
	first := NewLocalObservation(nil)
	one := ObservedHubAdvert("node-a", store, first)
	two := ObservedHubAdvert("node-a", store, first)
	if one.Snapshot.Generation == 0 || two.Snapshot.Generation != one.Snapshot.Generation {
		t.Fatalf("generations %d, %d", one.Snapshot.Generation, two.Snapshot.Generation)
	}
	if one.Snapshot.Sequence != 1 || two.Snapshot.Sequence != 2 {
		t.Fatalf("sequences %d, %d", one.Snapshot.Sequence, two.Snapshot.Sequence)
	}
	if other := ObservedHubAdvert("node-b", store, NewLocalObservation(nil)); other.Snapshot.Sequence != 1 {
		t.Fatalf("second observation continued at %d", other.Snapshot.Sequence)
	}
}

// Outside a cluster the page may name the hub by its node name; the
// service recognizes its own name without asking the process.
func TestServiceRecognizesItsOwnNameOutsideACluster(t *testing.T) {
	a := &Service{NodeName: "node-a"}
	if got := a.nodeKey("node-a"); got != "" {
		t.Fatalf("nodeKey(node-a) = %q, want the hub", got)
	}
	if got := a.nodeKey("node-b"); got != "node-b" {
		t.Fatalf("nodeKey(node-b) = %q", got)
	}
}

// An observation made without NewLocalObservation still numbers its
// snapshots: the first one fixes a nonzero generation the rest share.
func TestZeroObservationNumbersSnapshots(t *testing.T) {
	store := NewConfigStore(&config.Config{})
	observation := &LocalObservation{}
	one := ObservedHubAdvert("node-a", store, observation)
	two := ObservedHubAdvert("node-a", store, observation)
	if one.Snapshot.Generation == 0 || two.Snapshot.Generation != one.Snapshot.Generation {
		t.Fatalf("generations %d, %d", one.Snapshot.Generation, two.Snapshot.Generation)
	}
	if one.Snapshot.Sequence != 1 || two.Snapshot.Sequence != 2 {
		t.Fatalf("sequences %d, %d", one.Snapshot.Sequence, two.Snapshot.Sequence)
	}
}

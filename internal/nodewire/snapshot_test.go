package nodewire

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// The hub builds a snapshot for an advert that carries none from its
// harnesses and tags, and the snapshot says the hub synthesized it; an
// advert with a snapshot keeps the one the node reported.
func TestSynthesizeSaysTheHubBuiltTheSnapshot(t *testing.T) {
	now := time.Now()
	s := Synthesize(Advert{Node: "n", Harnesses: []Harness{{ID: "codex"}}, Capabilities: []string{"gpu"}}, now)
	if s.Source != "synthesized" || s.Node != "n" || len(s.Offers) != 2 {
		t.Fatalf("synthesized snapshot = %+v", s)
	}
	reported := &ability.Snapshot{Node: "n", Source: "node"}
	if got := Synthesize(Advert{Node: "n", Snapshot: reported}, now); got != reported {
		t.Fatalf("snapshot = %+v, want the one the node reported", got)
	}
}

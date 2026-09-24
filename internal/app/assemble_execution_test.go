package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/models"
)

// openedSessions records where probe sessions were opened and opens none.
type openedSessions struct{ at []string }

func (o *openedSessions) OpenSession(_ context.Context, at harness.Placement, _, workdir string, _ []acp.MCPServer) (harness.Runner, error) {
	o.at = append(o.at, at.Node+"/"+at.Harness+" in "+workdir)
	return nil, errors.New("no session in tests")
}

func (o *openedSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

// The coordinator's probe asks one harness in its machine's probe
// directory, refuses a machine with none, and probes every endpoint again
// even when its model is already known.
func TestModelProbeUsesEachMachinesProbeDirectory(t *testing.T) {
	opened := &openedSessions{}
	seen := models.New()
	seen.Observe(models.Observation{Node: "node-a", Harness: "kimi", Current: "k2"})
	probe := modelProbe{
		prober: models.NewProber(opened, seen, nil),
		probeDir: func(node string) string {
			if node == "node-a" {
				return "/state/probe"
			}
			return ""
		},
		endpoints: func(context.Context) []models.Endpoint {
			return []models.Endpoint{{Node: "node-a", Harness: "kimi", Workdir: "/state/probe"}}
		},
	}

	err := probe.Probe(t.Context(), "node-b", "kimi")
	if err == nil || !strings.Contains(err.Error(), "no state dir known for node-b") {
		t.Fatalf("probe on a machine without a state dir: %v", err)
	}
	if len(opened.at) != 0 {
		t.Fatalf("opened %v on a machine without a state dir", opened.at)
	}
	if err := probe.Probe(t.Context(), "node-a", "kimi"); err == nil {
		t.Fatal("probe hid the session error")
	}
	results := probe.ProbeAll(t.Context())
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("probe all = %+v, want the known harness probed again", results)
	}
	want := []string{"node-a/kimi in /state/probe", "node-a/kimi in /state/probe"}
	if strings.Join(opened.at, ",") != strings.Join(want, ",") {
		t.Fatalf("opened = %v, want %v", opened.at, want)
	}
}

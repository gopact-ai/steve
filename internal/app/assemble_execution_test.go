package app

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestExecutionAssemblyInstallsRecoverySinkBeforeConsumersStart(t *testing.T) {
	root := t.TempDir()
	book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	background := newApplicationBackground(ctx)
	defer background.Close()
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	catalog, err := agent.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book, attempt.CheckDeclarationsTx)
	nodes := node.NewRegistry("fixture-hub", nil)
	defer nodes.Close()
	skillmap, err := skills.Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	live := &skills.Live{Map: skillmap}
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(root, "state"), HomePath: filepath.Join(root, "home"), OwnerID: "owner", PromptTimeout: config.Duration(time.Minute)}}
	profile, err := prepareApplicationMemoryWithSettings(ctx, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := assembleExecution(&assemblyInput{}, &runtimeValues{book: book, catalog: catalog, cfg: cfg, ctx: ctx, background: background, live: live, manager: manager, nodeName: "fixture-hub"}, &ledgerValues{attempts: attempt.New(book), store: store}, &homeValues{assembler: capability.NewAssembler(nil), profile: profile}, &fleetValues{nodes: nodes, projects: projects, fleet: roster.New(catalog)}, &modelsValues{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = assembled.Artifacts().LandRecoveryResolutionOnce(ctx, attempt.RecoveryResolutionRef{Episode: "missing-episode"}, "missing-artifact", artifact.Source{Execution: &task.ExecutionToken{TaskID: "missing-task", Epoch: 1}, AttemptID: "missing-attempt"})
	if err == nil || strings.Contains(err.Error(), "requires its owner driver") {
		t.Fatalf("assembly started consumers without the owner-controlled recovery sink: %v", err)
	}
}

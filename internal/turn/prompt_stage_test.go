package turn

import (
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

// A turn spends its first seconds on the platform's own work — settling a
// directory, assembling capabilities, starting a cold agent — with
// nothing to show. Each step says which one it is, in order, and none is
// reported once the agent has the prompt.
func TestPromptReportsItsPreparationSteps(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(map[string]harness.Config{"codex": {Command: bin, Permission: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)

	var mu sync.Mutex
	var stages []view.Stage
	var running bool
	var afterRunning []view.Stage
	request := func(message string) Request {
		return Request{ConversationID: "chat", MessageID: message, Input: message,
			OnPhase: func(p view.Phase) {
				mu.Lock()
				defer mu.Unlock()
				if p == view.PhaseRunning {
					running = true
				}
			},
			OnStage: func(s view.Stage) {
				mu.Lock()
				defer mu.Unlock()
				stages = append(stages, s)
				if running {
					afterRunning = append(afterRunning, s)
				}
			},
		}
	}
	if _, err := coordinator.Handle(t.Context(), request("first")); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// The baseline snapshot only happens where a project keeps artifacts,
	// so the order is checked rather than the exact list.
	order := []view.Stage{view.StageWorkspace, view.StageCapabilities, view.StagePlacement, view.StageSnapshot, view.StageSession}
	required := []view.Stage{view.StageWorkspace, view.StageCapabilities, view.StagePlacement, view.StageSession}
	mu.Lock()
	first := append([]view.Stage(nil), stages...)
	late := append([]view.Stage(nil), afterRunning...)
	stages, running = nil, false
	mu.Unlock()
	at := func(stage view.Stage) int {
		for i, s := range first {
			if s == stage {
				return i
			}
		}
		return -1
	}
	for _, stage := range required {
		if at(stage) < 0 {
			t.Fatalf("preparation steps = %v, missing %q", first, stage)
		}
	}
	previous := -1
	for _, stage := range order {
		if i := at(stage); i >= 0 {
			if i < previous {
				t.Fatalf("preparation steps out of order: %v, want the order of %v", first, order)
			}
			previous = i
		}
	}
	if len(late) != 0 {
		t.Fatalf("steps reported after the agent started working: %v", late)
	}

	// The second turn reopens the session it already has, and says so
	// rather than claiming a cold start.
	if _, err := coordinator.Handle(t.Context(), request("second")); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	mu.Lock()
	second := append([]view.Stage(nil), stages...)
	mu.Unlock()
	if len(second) == 0 || second[len(second)-1] != view.StageResume {
		t.Fatalf("resumed turn steps = %v, want them to end with %q", second, view.StageResume)
	}
}

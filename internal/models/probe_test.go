package models

import (
	"context"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/view"
)

// reportingRunner reports its settings but exposes no setters: a probe
// only reads, so reading is all it may require of a runner.
type reportingRunner struct{ settings view.Settings }

func (r *reportingRunner) ID() string { return "s1" }
func (r *reportingRunner) Prompt(context.Context, string, func(view.Progress)) (string, []string, error) {
	return "", nil, nil
}
func (r *reportingRunner) Cancel(context.Context) error { return nil }
func (r *reportingRunner) Abort()                       {}
func (r *reportingRunner) Settings() view.Settings      { return r.settings }

type stubOpener struct{ runner harness.Runner }

func (o stubOpener) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return o.runner, nil
}

func (o stubOpener) CloseSession(context.Context, harness.Placement, string) error { return nil }

func TestProbeReadsSettingsFromRunnerWithoutSetters(t *testing.T) {
	runner := &reportingRunner{settings: view.Settings{
		Model:   "gpt-9",
		Models:  []string{"gpt-9", "gpt-8"},
		Adapter: "acp-1",
	}}
	book := New()
	prober := NewProber(stubOpener{runner: runner}, book, nil)

	obs, err := prober.Probe(context.Background(), Endpoint{Harness: "codex"})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if obs.Current != "gpt-9" || obs.Version != "acp-1" || len(obs.Available) != 2 {
		t.Fatalf("probe dropped the reported settings: %+v", obs)
	}
}

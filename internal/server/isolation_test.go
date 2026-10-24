package server

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/runtime"
)

func TestHarnessesRunInTheirOwnHomesWithoutChangingTheDeclaration(t *testing.T) {
	state := t.TempDir()
	cfg := &config.Config{Harnesses: map[string]config.Harness{harness.Codex: {Command: "codex-acp", Env: []string{"A=1"}}}}
	running := isolated(cfg, state)
	env := running.Harnesses[harness.Codex].Env
	want := harness.EnvCodexHome + "=" + runtime.CodexHome(state)
	if !slices.Contains(env, want) || !slices.Contains(env, "A=1") || !strings.HasPrefix(runtime.CodexHome(state), filepath.Clean(state)) {
		t.Fatalf("codex env = %v, want %s", env, want)
	}
	if len(cfg.Harnesses[harness.Codex].Env) != 1 {
		t.Fatal("the declaration was changed")
	}
}

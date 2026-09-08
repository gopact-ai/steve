package admin

import (
	"path/filepath"
	"slices"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/runtime"
)

// harnessRuntimeConfig adds derived process settings without changing the
// declaration later saved by administrative edits.
func HarnessRuntimeConfig(cfg *config.Config) *config.Config {
	running := *cfg
	running.Harnesses = make(map[string]config.Harness, len(cfg.Harnesses))
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	for id, item := range cfg.Harnesses {
		item.Args = slices.Clone(item.Args)
		item.Env = runtime.ApplyEnv(slices.Clone(item.Env), id, stateDir)
		running.Harnesses[id] = item
	}
	return &running
}

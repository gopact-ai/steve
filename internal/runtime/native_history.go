package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

// PrepareNativeHistory publishes one execution's isolated native home. Its
// selected transcript comes from the immutable import; model access and skills
// come from the currently admitted runtime, including a pinned plugin profile.
func PrepareNativeHistory(ctx context.Context, stateDir, execution string, ref nativehistory.Reference, cfg harness.Config) (harness.Config, error) {
	if !strings.HasPrefix(execution, "ns_") || strings.ContainsAny(execution, "/\\") {
		return harness.Config{}, errors.New("native history needs a managed execution identity")
	}
	profiles := PluginProfiles{StateDir: stateDir}
	key, source := profiles.nativeHome(ref.Harness, cfg.Env)
	if key == "" {
		return harness.Config{}, nativehistory.ErrUnsupported
	}
	parent := filepath.Join(stateDir, "native-runtimes")
	release, err := nativehistory.LockStorage(ctx, parent)
	if err != nil {
		return harness.Config{}, err
	}
	defer release()
	dest := filepath.Join(parent, execution)
	if _, err := os.Lstat(dest); err == nil {
		return harness.Config{}, errors.New("native import runtime already exists; reconcile its original execution")
	} else if !errors.Is(err, os.ErrNotExist) {
		return harness.Config{}, err
	}
	if err := nativehistory.CheckStorage(ctx, parent, 1, nativehistory.MaxSnapshotBytes); err != nil {
		return harness.Config{}, err
	}
	stage, err := os.MkdirTemp(parent, ".prepare-")
	if err != nil {
		return harness.Config{}, err
	}
	defer os.RemoveAll(stage)
	home := filepath.Join(stage, "home")
	if err := nativehistory.Materialize(ctx, filepath.Join(stateDir, "native-imports"), ref, home); err != nil {
		return harness.Config{}, err
	}
	switch ref.Harness {
	case harness.Codex:
		err = PrepareCodex(home, source)
	case harness.ClaudeCode:
		err = PrepareClaude(home, source)
	case harness.Dsh:
		err = PrepareDsh(home, source)
	case harness.Grok:
		err = PrepareGrok(home, source)
	default:
		err = nativehistory.ErrUnsupported
	}
	if err != nil {
		return harness.Config{}, err
	}
	if err := copyProfileSkills(ctx, filepath.Join(source, "skills"), filepath.Join(home, "skills")); err != nil {
		return harness.Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return harness.Config{}, err
	}
	if err := nativehistory.CheckStorage(ctx, parent, 0, 0); err != nil {
		return harness.Config{}, err
	}
	if err := os.Rename(stage, dest); err != nil {
		return harness.Config{}, err
	}
	cfg.Env = replaceProfileEnv(cfg.Env, key, filepath.Join(dest, "home"))
	if HasEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR") {
		cfg.Env = replaceProfileEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR", filepath.Join(dest, "home", "skills"))
	}
	return cfg, nil
}

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// ResumeNativeHistory reuses the admitted managed home, including inputs made
// since import. Re-materializing the original snapshot would lose that context.
// The node must hold the exclusive resume claim before calling this helper.
func ResumeNativeHistory(ctx context.Context, stateDir, execution string, ref nativehistory.Reference, cfg harness.Config) (harness.Config, error) {
	if !nativehistory.StorageSupported {
		return harness.Config{}, nativehistory.ErrUnsupported
	}
	if !nodewire.IsManagedSession(execution) || strings.ContainsAny(execution, "/\\") {
		return harness.Config{}, errors.New("native history needs a managed execution identity")
	}
	key, _ := (PluginProfiles{StateDir: stateDir}).nativeHome(ref.Harness, cfg.Env)
	if key == "" {
		return harness.Config{}, nativehistory.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return harness.Config{}, err
	}
	parent := filepath.Join(stateDir, "native-runtimes")
	home := filepath.Join(parent, execution, "home")
	for _, path := range []string{parent, filepath.Dir(home), home} {
		info, err := os.Lstat(path)
		if err != nil {
			return harness.Config{}, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return harness.Config{}, errors.New("native resume home must be a private directory")
		}
	}
	cfg.Env = replaceProfileEnv(cfg.Env, key, home)
	if HasEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR") {
		cfg.Env = replaceProfileEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR", filepath.Join(home, "skills"))
	}
	return cfg, nil
}

// PrepareNativeHistory publishes one execution's isolated native home. Its
// selected transcript comes from the immutable import; model access and skills
// come from the currently admitted runtime, including a pinned plugin profile.
func PrepareNativeHistory(ctx context.Context, stateDir, execution string, ref nativehistory.Reference, cfg harness.Config) (harness.Config, error) {
	if !nodewire.IsManagedSession(execution) || strings.ContainsAny(execution, "/\\") {
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
		err = nativehistory.PrepareDshRuntime(ctx, home)
		if err == nil {
			err = PrepareDsh(home, source)
		}
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
	if err := syncNativeRuntime(ctx, stage); err != nil {
		return harness.Config{}, err
	}
	if err := os.Rename(stage, dest); err != nil {
		return harness.Config{}, err
	}
	if err := syncNativePath(parent); err != nil {
		_ = os.RemoveAll(dest) // No session can use it before preparation returns.
		return harness.Config{}, err
	}
	cfg.Env = replaceProfileEnv(cfg.Env, key, filepath.Join(dest, "home"))
	if HasEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR") {
		cfg.Env = replaceProfileEnv(cfg.Env, "STEVE_PLUGIN_SKILLS_DIR", filepath.Join(dest, "home", "skills"))
	}
	return cfg, nil
}

// Preparation writes access settings and skills after Materialize has synced
// history. Persist those files and directories before the node commits a record.
func syncNativeRuntime(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return syncNativePath(path)
	})
}
func syncNativePath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

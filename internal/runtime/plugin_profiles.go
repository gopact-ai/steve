package runtime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/skills"
)

// PluginProfiles materializes a session's own native home. Existing runtime
// directories are never rewritten by another session or package update.
type PluginProfiles struct {
	Store    *plugins.Store
	StateDir string
}

func (p PluginProfiles) Prepare(ctx context.Context, id string, selection plugins.Selection, cfg harness.Config) (plugins.RuntimeRecord, error) {
	native := plugins.RuntimeConfig{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, ProcessDir: cfg.ProcessDir, Permission: cfg.Permission}
	return p.Store.PrepareRuntime(ctx, id, selection, native, p.materialize)
}

func (p PluginProfiles) materialize(ctx context.Context, dir string, record plugins.RuntimeRecord) (string, error) {
	selection := record.Ref.Selection
	key, source := p.nativeHome(selection.Harness, record.Config.Env)
	home := filepath.Join(dir, "home")
	var err error
	switch selection.Harness {
	case harness.Codex:
		err = PrepareCodex(home, source)
	case harness.ClaudeCode:
		err = PrepareClaude(home, source)
	case harness.Grok:
		err = PrepareGrok(home, source)
	case harness.Kimi:
		err = PrepareKimi(home, source)
	default:
		err = os.MkdirAll(filepath.Join(home, "skills"), 0700)
	}
	if err != nil {
		return "", err
	}
	if key != "" {
		if err := copyProfileSkills(ctx, filepath.Join(source, "skills"), filepath.Join(home, "skills"), selection.ExcludedSkills...); err != nil {
			return "", err
		}
	}
	if _, err := p.Store.MaterializeRuntimeSkills(ctx, selection, filepath.Join(home, "skills")); err != nil {
		return "", err
	}
	return plugins.SkillsDigest(ctx, filepath.Join(home, "skills"))
}

func (p PluginProfiles) Config(record plugins.RuntimeRecord) (harness.Config, error) {
	loaded, err := p.Store.Runtime(record.Ref)
	if err != nil {
		return harness.Config{}, err
	}
	dir := p.Store.RuntimeDir(record.Ref.ID)
	digest, err := plugins.SkillsDigest(context.Background(), filepath.Join(dir, "home", "skills"))
	if err != nil {
		return harness.Config{}, err
	}
	if digest != loaded.SkillsHash {
		return harness.Config{}, plugins.ErrIntegrity
	}
	native := loaded.Config.Clone()
	key, _ := p.nativeHome(record.Ref.Selection.Harness, native.Env)
	if key != "" {
		native.Env = replaceProfileEnv(native.Env, key, filepath.Join(dir, "home"))
	}
	native.Env = replaceProfileEnv(native.Env, "STEVE_PLUGIN_SKILLS_DIR", filepath.Join(dir, "home", "skills"))
	return harness.Config{Command: native.Command, Args: native.Args, Env: native.Env, ProcessDir: native.ProcessDir, Permission: native.Permission}, nil
}

func (p PluginProfiles) nativeHome(id string, env []string) (string, string) {
	key, source := "", ""
	switch id {
	case harness.Codex:
		key, source = harness.EnvCodexHome, CodexHome(p.StateDir)
	case harness.ClaudeCode:
		key, source = harness.EnvClaudeConfigDir, ClaudeHome(p.StateDir)
	case harness.Grok:
		key, source = harness.EnvGrokHome, GrokHome(p.StateDir)
	case harness.Kimi:
		key, source = harness.EnvKimiCodeHome, KimiHome(p.StateDir)
	}
	if key != "" {
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, key+"="); ok {
				source = value
			}
		}
	}
	return key, source
}

func replaceProfileEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}

func copyProfileSkills(ctx context.Context, source, dest string, excluded ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	refs := make([]skills.Ref, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || slices.Contains(excluded, entry.Name()) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		refs = append(refs, skills.Ref{Name: entry.Name(), Path: resolved})
	}
	bundle, err := skills.Pack(refs)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = skills.Unpack(bundle.Data, dest)
	return err
}

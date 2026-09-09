package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

func profileFixture(t *testing.T) (PluginProfiles, plugins.Selection, harness.Config) {
	t.Helper()
	state := t.TempDir()
	store := &plugins.Store{Dir: filepath.Join(state, "plugins")}
	bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "examples/plugins/github"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(t.Context(), plugins.InstallRequest{CommandID: "package", ExpectedDigest: bundle.Digest, Bundle: bundle, Source: plugins.Source{Kind: "bundle", Location: bundle.Digest}}); err != nil {
		t.Fatal(err)
	}
	secret, err := store.PutSecret(t.Context(), "github", "private")
	if err != nil {
		t.Fatal(err)
	}
	d := plugins.Deployment{Installation: "github", PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Node: "node", Projects: []string{"p"}, Configuration: plugins.Configuration{Secrets: map[string]plugins.SecretRef{"token": secret.Reference}}}
	receipt, err := store.PrepareDeployment(t.Context(), d, plugins.Environment{})
	if err != nil {
		t.Fatal(err)
	}
	native := CodexHome(state)
	if err := os.MkdirAll(filepath.Join(native, "skills", "global"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "config.toml"), []byte("model = \"original\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "skills/global/SKILL.md"), []byte("original global skill"), 0600); err != nil {
		t.Fatal(err)
	}
	return PluginProfiles{Store: store, StateDir: state}, plugins.Selection{Project: "p", Node: "node", Harness: harness.Codex, Deployments: []string{receipt.Hash}}, harness.Config{Command: "test-agent", Env: []string{harness.EnvCodexHome + "=" + native}, Permission: "read"}
}

func TestPluginRuntimeRetainsOriginalHomeAcrossDefaultChanges(t *testing.T) {
	profiles, selection, cfg := profileFixture(t)
	first, err := profiles.Prepare(t.Context(), "session-one", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	original := profiles.Store.RuntimeDir(first.Ref.ID)
	if err := os.WriteFile(filepath.Join(CodexHome(profiles.StateDir), "config.toml"), []byte("model = \"changed\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(CodexHome(profiles.StateDir), "skills/global/SKILL.md"), []byte("changed global skill"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := PluginProfiles{Store: &plugins.Store{Dir: profiles.Store.Dir}, StateDir: profiles.StateDir}
	cfg.Command = "other-agent"
	replay, err := restarted.Prepare(t.Context(), "session-one", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Ref.ID != first.Ref.ID || replay.Config.Command != "test-agent" {
		t.Fatal("replay changed native launch identity")
	}
	for name, want := range map[string]string{"config.toml": "original", "skills/global/SKILL.md": "original global skill"} {
		raw, err := os.ReadFile(filepath.Join(original, "home", name))
		if err != nil || !strings.Contains(string(raw), want) {
			t.Fatalf("old home changed: %s %v", raw, err)
		}
	}
	active, err := restarted.Config(replay)
	if err != nil {
		t.Fatal(err)
	}
	if !HasEnv(active.Env, harness.EnvCodexHome) || !strings.Contains(strings.Join(active.Env, "\n"), original) {
		t.Fatal("launch still points at shared home")
	}
	next, err := profiles.Prepare(t.Context(), "session-two", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if next.Ref.ID == first.Ref.ID {
		t.Fatal("new session reused mutable native home")
	}
	raw, err := os.ReadFile(filepath.Join(profiles.Store.RuntimeDir(next.Ref.ID), "home", "skills/global/SKILL.md"))
	if err != nil || string(raw) != "changed global skill" {
		t.Fatalf("new runtime missed current defaults: %s %v", raw, err)
	}
	if err := os.WriteFile(filepath.Join(original, "home", "skills/global/SKILL.md"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Config(replay); !errors.Is(err, plugins.ErrIntegrity) {
		t.Fatalf("mutated old skill was accepted: %v", err)
	}
}

func TestAdoptedSkillIsExcludedOnlyFromTheNewPluginHome(t *testing.T) {
	profiles, selection, cfg := profileFixture(t)
	before, err := profiles.Prepare(t.Context(), "before-adoption", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	selection.ExcludedSkills = []string{"global"}
	after, err := profiles.Prepare(t.Context(), "after-adoption", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profiles.Store.RuntimeDir(after.Ref.ID), "home/skills/global")); !os.IsNotExist(err) {
		t.Fatalf("new profile still contains adopted source: %v", err)
	}
	for _, path := range []string{filepath.Join(CodexHome(profiles.StateDir), "skills/global/SKILL.md"), filepath.Join(profiles.Store.RuntimeDir(before.Ref.ID), "home/skills/global/SKILL.md")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original global skill" {
			t.Fatalf("adoption changed existing source/session: %s %v", data, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(profiles.Store.RuntimeDir(after.Ref.ID), "home/skills"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("replacement package skills missing: %v", err)
	}
}

func TestPluginRuntimePreparationKeepsIdentityAfterMaterializationFailure(t *testing.T) {
	profiles, selection, cfg := profileFixture(t)
	fault := errors.New("materialization interrupted")
	native := plugins.RuntimeConfig{Command: cfg.Command, Env: cfg.Env, Permission: cfg.Permission}
	failed, err := profiles.Store.PrepareRuntime(t.Context(), "retry", selection, native, func(context.Context, string, plugins.RuntimeRecord) (string, error) { return "", fault })
	if !errors.Is(err, fault) || failed.Ref.ID == "" {
		t.Fatalf("fault: %v", err)
	}
	recovered, err := profiles.Prepare(t.Context(), "retry", selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Ref.ID != failed.Ref.ID {
		t.Fatal("retry allocated another native home")
	}
	if _, err := profiles.Config(recovered); err != nil {
		t.Fatal(err)
	}
}

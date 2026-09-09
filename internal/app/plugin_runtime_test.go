package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plugins"
)

func runtimeTestBundle(t *testing.T, version, text string) plugins.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "skill"), 0700); err != nil {
		t.Fatal(err)
	}
	m := plugins.Manifest{Schema: plugins.Schema, API: plugins.API, ID: "test/runtime", Version: version, Description: "runtime fixture", Skills: map[string]string{"work": "skill"}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skill/SKILL.md"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := plugins.ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestPluginSessionsKeepOldVersionAcrossUpgradeAndGlobalRestart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	command := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	command.Dir = "../.."
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	state := t.TempDir()
	store := &plugins.Store{Dir: filepath.Join(state, "plugins")}
	library := &plugins.Library{Store: store, Ledger: book}
	original := runtimeTestBundle(t, "1.0.0", "PLUGIN_ORIGINAL")
	if _, err := library.Add(t.Context(), "p", original); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Harnesses: map[string]config.Harness{"mock": {Command: bin, Permission: "read"}}, Plugins: map[string]plugins.Installation{"work": {PackageID: original.Manifest.ID, Digest: original.Digest, Enabled: true, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}}}
	pool := &node.PluginRuntimePool{Store: store, StateDir: state}
	t.Cleanup(func() { pool.Close() })
	provider := &applicationPlugins{cfg: cfg, library: library, local: pool}
	manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetPluginRuntimes(provider)
	at := harness.Placement{Harness: "mock"}
	work := t.TempDir()
	prepare := func(id, upstream string, prior *plugins.RuntimeRef) *plugins.RuntimeRef {
		t.Helper()
		ref, err := manager.PreparePluginSession(t.Context(), harness.PluginPreparation{At: at, Project: "p", AttemptID: id, Upstream: upstream, Prior: prior})
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	first := prepare("a1", "", nil)
	one, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), first), at, "", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, _, err := one.Prompt(t.Context(), "hello", nil)
	if err != nil || !strings.Contains(answer, "PLUGIN_ORIGINAL") {
		t.Fatalf("first: %q %v", answer, err)
	}
	replacement := runtimeTestBundle(t, "2.0.0", "PLUGIN_REPLACEMENT")
	if _, err := library.Add(t.Context(), "p", replacement); err != nil {
		t.Fatal(err)
	}
	adminsvc.ConfigMu.Lock()
	item := cfg.Plugins["work"]
	item.Digest = replacement.Digest
	cfg.Plugins["work"] = item
	adminsvc.ConfigMu.Unlock()
	if err := manager.Restart(); err != nil {
		t.Fatal(err)
	}
	if one.(*harness.Session).Stopped() {
		t.Fatal("global skill restart stopped immutable plugin runtime")
	}
	continued := prepare("a2", one.ID(), first)
	resumed, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), continued), at, one.ID(), work, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, _, err = resumed.Prompt(t.Context(), "again", nil)
	if err != nil || !strings.Contains(answer, "PLUGIN_ORIGINAL") || strings.Contains(answer, "PLUGIN_REPLACEMENT") {
		t.Fatalf("old session changed: %q %v", answer, err)
	}
	second := prepare("a3", "", nil)
	if second.ID == first.ID {
		t.Fatal("upgrade reused runtime")
	}
	two, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), second), at, "", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, _, err = two.Prompt(t.Context(), "hello", nil)
	if err != nil || !strings.Contains(answer, "PLUGIN_REPLACEMENT") {
		t.Fatalf("new session missed upgrade: %q %v", answer, err)
	}
	denied, err := manager.PreparePluginSession(context.Background(), harness.PluginPreparation{At: at, Project: "unrelated", AttemptID: "a4"})
	if err != nil || denied != nil {
		t.Fatalf("package crossed project scope: %+v %v", denied, err)
	}
	if err := manager.CloseSession(t.Context(), at, one.ID()); err != nil {
		t.Fatal(err)
	}
}

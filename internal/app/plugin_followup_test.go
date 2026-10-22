package app

import (
	"errors"
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
	"github.com/gopact-ai/steve/internal/plugins/pluginledger"
)

// A follow-up turn in a plugin session reuses the runtime its first turn
// prepared. Between the turns the harness writes its own hidden state into
// the runtime's skills directory (Codex creates skills/.system/ on start) and
// the agent switches the project's branch; neither is plugin content, and
// neither may make the second turn fail.
func TestPluginFollowUpTurnKeepsPriorRuntime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	state := t.TempDir()
	store := &plugins.Store{Dir: filepath.Join(state, "plugins")}
	library := &pluginledger.Library{Store: store, Ledger: book}
	bundle := runtimeTestBundle(t, "1.0.0", "PLUGIN_ORIGINAL")
	if _, err := library.Add(t.Context(), "p", bundle); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Harnesses: map[string]config.Harness{"mock": {Command: bin, Permission: "read"}}, Plugins: map[string]plugins.Installation{"work": {PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Enabled: true, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}}}
	pool := &node.PluginRuntimePool{Store: store, StateDir: state}
	t.Cleanup(func() { pool.Close() })
	provider := &applicationPlugins{config: adminsvc.NewConfigStore(cfg), library: library, local: pool}
	manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetPluginRuntimes(provider)
	at := harness.Placement{Harness: "mock"}
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		command.Dir = work
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "README")
	git("commit", "-q", "-m", "first")

	first, err := manager.PreparePluginSession(t.Context(), harness.PluginPreparation{At: at, Project: "p", AttemptID: "turn-1"})
	if err != nil || first == nil {
		t.Fatalf("first turn: %+v %v", first, err)
	}
	session, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), first), at, "", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer, _, err := session.Prompt(t.Context(), "hello", nil); err != nil || !strings.Contains(answer, "PLUGIN_ORIGINAL") {
		t.Fatalf("first turn: %q %v", answer, err)
	}

	// What the harness and the agent leave behind after the first turn.
	skills := filepath.Join(store.RuntimeDir(first.ID), "home", "skills")
	for name, body := range map[string]string{".system/.codex-system-skills.marker": "x", ".system/skill-creator/SKILL.md": "harness skill", ".DS_Store": "x"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(skills, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skills, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("checkout", "-q", "-b", "steve/feature")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := manager.PreparePluginSession(t.Context(), harness.PluginPreparation{At: at, Project: "p", AttemptID: "turn-2", Upstream: session.ID(), Prior: first})
	if err != nil {
		t.Fatalf("follow-up turn refused its own runtime: %v", err)
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("follow-up turn changed runtime: %+v", second)
	}
	resumed, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), second), at, session.ID(), work, nil)
	if err != nil {
		t.Fatalf("follow-up turn could not reopen its session: %v", err)
	}
	if answer, _, err := resumed.Prompt(t.Context(), "again", nil); err != nil || !strings.Contains(answer, "PLUGIN_ORIGINAL") {
		t.Fatalf("follow-up turn: %q %v", answer, err)
	}
	if err := manager.CloseSession(t.Context(), at, session.ID()); err != nil {
		t.Fatal(err)
	}
}

// A follow-up turn whose runtime skills really changed is refused with words
// that say which runtime and what changed, not a bare package error.
func TestPluginFollowUpTurnNamesChangedRuntime(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	state := t.TempDir()
	store := &plugins.Store{Dir: filepath.Join(state, "plugins")}
	library := &pluginledger.Library{Store: store, Ledger: book}
	bundle := runtimeTestBundle(t, "1.0.0", "PLUGIN_ORIGINAL")
	if _, err := library.Add(t.Context(), "p", bundle); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Harnesses: map[string]config.Harness{"mock": {Command: "mock-agent", Permission: "read"}}, Plugins: map[string]plugins.Installation{"work": {PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Enabled: true, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}}}
	pool := &node.PluginRuntimePool{Store: store, StateDir: state}
	t.Cleanup(func() { pool.Close() })
	provider := &applicationPlugins{config: adminsvc.NewConfigStore(cfg), library: library, local: pool}
	at := harness.Placement{Harness: "mock"}
	first, err := provider.PreparePluginSession(t.Context(), harness.PluginPreparation{At: at, Project: "p", AttemptID: "turn-1"})
	if err != nil || first == nil {
		t.Fatalf("first turn: %+v %v", first, err)
	}
	skills := filepath.Join(store.RuntimeDir(first.ID), "home", "skills")
	entries, err := os.ReadDir(skills)
	if err != nil || len(entries) == 0 {
		t.Fatalf("runtime skills: %v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(skills, entries[0].Name(), "SKILL.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = provider.PreparePluginSession(t.Context(), harness.PluginPreparation{At: at, Project: "p", AttemptID: "turn-2", Upstream: "ps_" + first.ID + ":native", Prior: first})
	if !errors.Is(err, plugins.ErrIntegrity) || !strings.Contains(err.Error(), first.ID) || !strings.Contains(err.Error(), "skills changed") {
		t.Fatalf("changed runtime refusal is not actionable: %v", err)
	}
}

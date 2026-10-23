package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/pihome"
)

func TestPrepareSelectedDoesNotAccessUnselectedToolHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	if err := PrepareSelected(stateDir, nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		t.Fatalf("empty selection created runtime files: %v, %v", entries, err)
	}
	// Reading an unselected tool's config would fail because it is a directory.
	for _, tool := range []string{".codex", ".claude"} {
		if err := os.MkdirAll(filepath.Join(home, tool, "config.toml"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareSelected(stateDir, []string{harness.Pi}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{CodexHome(stateDir), ClaudeHome(stateDir)} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("unselected runtime %s was touched: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(PiHome(stateDir), "skills")); err != nil {
		t.Fatal(err)
	}
}

// A retired harness id selects nothing: an installation that still names
// one prepares the others and starts.
func TestPrepareSelectedIgnoresRetiredHarnesses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	if err := PrepareSelected(stateDir, []string{"kimi", "grok", "dsh", harness.Codex}); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"kimi", "grok", "dsh"} {
		if _, err := os.Lstat(filepath.Join(stateDir, "runtimes", retired)); !os.IsNotExist(err) {
			t.Fatalf("retired runtime %s was prepared: %v", retired, err)
		}
	}
	if got := SelectedSkillDests(stateDir, []string{"kimi", harness.Codex}); len(got) != 1 {
		t.Fatalf("retired harness got skills: %v", got)
	}
}

func TestSelectedSkillDestsOnlyNamesSelectedRuntimes(t *testing.T) {
	stateDir := t.TempDir()
	got := SelectedSkillDests(stateDir, []string{harness.Pi, "custom", harness.Pi, harness.Codex})
	if len(got) != 2 || got[0] != filepath.Join(PiHome(stateDir), "skills") || got[1] != filepath.Join(CodexHome(stateDir), "skills") {
		t.Fatalf("selected skill directories = %v", got)
	}
}

func TestFilterCodexConfigDropsSkillsKeepsModel(t *testing.T) {
	src := []byte(`
model = "gpt-5.4"
model_provider = "cpa"

[model_providers.cpa]
name = "cpa"
base_url = "https://example.invalid"

[[skills.config]]
path = "/tmp/operator-skill"

[plugins]
enabled = true

[features]
apps = true
plugins = true
remote_plugin = true

[mcp_servers.github]
command = "npx"

[projects."/tmp"]
trust = true

notify = ["echo", "hi"]

[tui]
theme = "dark"
`)
	got := string(FilterCodexConfig(src))
	if !strings.Contains(got, `model = "gpt-5.4"`) || !strings.Contains(got, `model_provider = "cpa"`) {
		t.Fatalf("dropped model settings: %s", got)
	}
	if !strings.Contains(got, "[model_providers.cpa]") {
		t.Fatalf("dropped provider: %s", got)
	}
	for _, banned := range []string{"skills.config", "[plugins]", "mcp_servers", "[projects", "notify", "[tui]"} {
		if strings.Contains(got, banned) {
			t.Fatalf("kept %q: %s", banned, got)
		}
	}
	assertCodexNativeDiscoveryDisabled(t, got)
	for _, empty := range [][]byte{nil, []byte("[features]\napps = true\nplugins = true\nremote_plugin = true\n")} {
		assertCodexNativeDiscoveryDisabled(t, string(FilterCodexConfig(empty)))
	}
}

func assertCodexNativeDiscoveryDisabled(t *testing.T, config string) {
	t.Helper()
	if strings.Count(config, "[features]") != 1 {
		t.Fatalf("isolated config needs one explicit feature policy: %s", config)
	}
	for _, feature := range []string{"apps", "plugins", "remote_plugin"} {
		if !strings.Contains(config, feature+" = false") || strings.Contains(config, feature+" = true") {
			t.Fatalf("ambient %s discovery remains enabled: %s", feature, config)
		}
	}
}

func TestPrepareCodexLinksAuthAndIsolatesSkills(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte("model = \"gpt-5.4\"\n[[skills.config]]\npath = \"/tmp/x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "codex")
	if err := PrepareCodex(dest, src); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dest, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(src, "auth.json") {
		t.Fatalf("auth link = %q", target)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "gpt-5.4") || strings.Contains(body, "skills.config") {
		t.Fatalf("isolated config = %s", body)
	}
	assertCodexNativeDiscoveryDisabled(t, body)
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatalf("skills dir: %v", err)
	}
}

func TestCodexLoginAfterRuntimePreparation(t *testing.T) {
	source := t.TempDir()
	// A plugin profile and an imported native session retain their own homes.
	// All of them must see a later login without being prepared again.
	homes := []string{source}
	for range 3 {
		home := t.TempDir()
		if err := PrepareCodex(home, homes[len(homes)-1]); err != nil {
			t.Fatal(err)
		}
		homes = append(homes, home)
		config, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		assertCodexNativeDiscoveryDisabled(t, string(config))
	}
	for _, value := range []string{"first-login", "refreshed-login"} {
		temp := filepath.Join(source, "new-auth")
		if err := os.WriteFile(temp, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, filepath.Join(source, "auth.json")); err != nil {
			t.Fatal(err)
		}
		for _, home := range homes[1:] {
			raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
			if err != nil || string(raw) != value {
				t.Fatalf("prepared runtime did not observe login: %q, %v", raw, err)
			}
		}
	}
	if err := os.Remove(filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	for _, home := range homes[1:] {
		if _, err := os.ReadFile(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
			t.Fatalf("prepared runtime retained logged-out credentials: %v", err)
		}
	}
}

func TestApplyEnvDoesNotOverrideExisting(t *testing.T) {
	stateDir := t.TempDir()
	got := ApplyEnv(nil, harness.Codex, stateDir)
	if !HasEnv(got, harness.EnvCodexHome) || got[0] != harness.EnvCodexHome+"="+CodexHome(stateDir) {
		t.Fatalf("codex env = %v", got)
	}
	kept := []string{harness.EnvCodexHome + "=/custom"}
	if out := ApplyEnv(kept, harness.Codex, stateDir); len(out) != 1 || out[0] != kept[0] {
		t.Fatalf("overrode existing: %v", out)
	}
	claude := ApplyEnv(nil, harness.ClaudeCode, stateDir)
	if !HasEnv(claude, harness.EnvClaudeConfigDir) {
		t.Fatalf("claude env = %v", claude)
	}
	// pi starts from the read-only home until a launch chooses by policy.
	pi := ApplyEnv(nil, harness.Pi, stateDir)
	if !slices.Contains(pi, harness.EnvPiAgentDir+"="+filepath.Join(PiHome(stateDir), "read-only")) || !slices.Contains(pi, pihome.EnvHomes+"="+PiHome(stateDir)) {
		t.Fatalf("pi env = %v", pi)
	}
	own := []string{harness.EnvPiAgentDir + "=/operator/pi"}
	if out := ApplyEnv(own, harness.Pi, stateDir); len(out) != 1 || out[0] != own[0] {
		t.Fatalf("overrode the operator's pi home: %v", out)
	}
	for _, retired := range []string{"kimi", "grok", "dsh"} {
		if out := ApplyEnv(nil, retired, stateDir); out != nil {
			t.Fatalf("retired harness %s env = %v", retired, out)
		}
	}
	if out := ApplyEnv(nil, "other", stateDir); out != nil {
		t.Fatalf("unknown harness env = %v", out)
	}
}

func TestPrepareCreatesEveryHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	if err := Prepare(stateDir); err != nil {
		t.Fatal(err)
	}
	for _, dest := range SkillDests(stateDir) {
		if info, err := os.Stat(dest); err != nil || !info.IsDir() {
			t.Fatalf("dest %s: %v", dest, err)
		}
	}
}

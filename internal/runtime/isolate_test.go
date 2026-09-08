package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
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
	for _, tool := range []string{".codex", ".grok"} {
		if err := os.MkdirAll(filepath.Join(home, tool, "config.toml"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareSelected(stateDir, []string{harness.Kimi}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{CodexHome(stateDir), ClaudeHome(stateDir), GrokHome(stateDir)} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("unselected runtime %s was touched: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(KimiHome(stateDir), "skills")); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedSkillDestsOnlyNamesSelectedRuntimes(t *testing.T) {
	stateDir := t.TempDir()
	got := SelectedSkillDests(stateDir, []string{harness.Kimi, "custom", harness.Kimi, harness.Codex})
	if len(got) != 2 || got[0] != filepath.Join(KimiHome(stateDir), "skills") || got[1] != filepath.Join(CodexHome(stateDir), "skills") {
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
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatalf("skills dir: %v", err)
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
	grok := ApplyEnv(nil, harness.Grok, stateDir)
	if !HasEnv(grok, harness.EnvGrokHome) || grok[0] != harness.EnvGrokHome+"="+GrokHome(stateDir) {
		t.Fatalf("grok env = %v", grok)
	}
	kimi := ApplyEnv(nil, harness.Kimi, stateDir)
	if !HasEnv(kimi, harness.EnvKimiCodeHome) || kimi[0] != harness.EnvKimiCodeHome+"="+KimiHome(stateDir) {
		t.Fatalf("kimi env = %v", kimi)
	}
	if out := ApplyEnv(nil, "other", stateDir); out != nil {
		t.Fatalf("unknown harness env = %v", out)
	}
}

func TestPrepareGrokCopiesAuthAndDisablesCompat(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "grok")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/missing-auth", filepath.Join(dest, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareGrok(dest, src); err != nil {
		t.Fatal(err)
	}
	rawAuth, err := os.ReadFile(filepath.Join(dest, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAuth) != `{"token":"x"}` {
		t.Fatalf("auth copy = %s", rawAuth)
	}
	if _, err := os.Readlink(filepath.Join(dest, "auth.json")); err == nil {
		t.Fatal("grok auth should be a regular file, not a symlink")
	}
	raw, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "[compat.claude]") || !strings.Contains(body, "skills = false") {
		t.Fatalf("isolated grok config = %s", body)
	}
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatalf("skills dir: %v", err)
	}
}

func TestPrepareKimiLinksCredentials(t *testing.T) {
	src := t.TempDir()
	for _, dir := range []string{"credentials", "oauth"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "kimi")
	if err := PrepareKimi(dest, src); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"credentials", "oauth"} {
		target, err := os.Readlink(filepath.Join(dest, dir))
		if err != nil {
			t.Fatal(err)
		}
		if target != filepath.Join(src, dir) {
			t.Fatalf("%s link = %q", dir, target)
		}
	}
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatalf("skills dir: %v", err)
	}
	// No operator config: Kimi starts from its own defaults.
	if _, err := os.Stat(filepath.Join(dest, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config.toml without an operator config: %v", err)
	}
}

func TestPrepareKimiInheritsProvidersNotHooks(t *testing.T) {
	src := t.TempDir()
	config := `default_model = "kimi-code/k3"

[[hooks]]
event = "PreToolUse"
command = "/home/op/.orca/kimi-hook.sh"

[providers."managed:kimi-code"]
type = "kimi"
base_url = "https://api.kimi.com/coding/v1"
api_key = "sk-test"

[providers."managed:kimi-code".oauth]
storage = "file"
key = "kimi-code"

[models."kimi-code/k3"]
provider = "managed:kimi-code"
model = "k3"
`
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "kimi")
	if err := PrepareKimi(dest, src); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`default_model = "kimi-code/k3"`, `[providers."managed:kimi-code"]`, `[providers."managed:kimi-code".oauth]`, `[models."kimi-code/k3"]`, `api_key = "sk-test"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("isolated config lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "hooks") || strings.Contains(string(got), "kimi-hook.sh") {
		t.Fatalf("isolated config kept the operator hooks:\n%s", got)
	}
}

func TestFilterKimiConfigDropsTerminalKeepsModelAccess(t *testing.T) {
	src := []byte(`default_model = "kimi-code/k3"
default_thinking = true
theme = "dark"
merge_all_available_skills = true
extra_skill_dirs = [
  "/home/op/skills",
]
telemetry = true

[[hooks]]
event = "SessionStart"
command = "/home/op/hook"

[models."kimi-code/k3"]
provider = "managed:kimi-code"
model = "k3"

[providers."managed:kimi-code"]
type = "kimi"
base_url = "https://api.kimi.com/coding/v1"

[providers."managed:kimi-code".oauth]
storage = "file"

[mcp_servers.github]
command = "npx"

[[mcp]]
name = "docs"

[thinking]
enabled = true

[loop_control]
max_steps_per_turn = 1_000

[tui]
theme = "dark"

[[cron]]
schedule = "* * * * *"

# >>> managed hooks (do not edit) >>>
[[hooks]]
event = "Stop"
command = "/home/op/hook"
# <<< managed hooks <<<
`)
	got := string(FilterKimiConfig(src))
	if strings.Contains(got, "managed hooks") {
		t.Fatalf("kept a comment that annotated dropped hooks: %s", got)
	}
	for _, want := range []string{`default_model = "kimi-code/k3"`, `default_thinking = true`, `telemetry = true`, `[models."kimi-code/k3"]`, `[providers."managed:kimi-code"]`, `[providers."managed:kimi-code".oauth]`, `[thinking]`, `[loop_control]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("dropped %q: %s", want, got)
		}
	}
	for _, banned := range []string{"hooks", "/home/op", "extra_skill_dirs", "merge_all_available_skills", `theme = "dark"`, "mcp", "[tui]", "cron"} {
		if strings.Contains(got, banned) {
			t.Fatalf("kept %q: %s", banned, got)
		}
	}
	if string(FilterKimiConfig(nil)) != "# generated by steve — isolated from the operator Kimi Code home\n" {
		t.Fatalf("empty config = %q", FilterKimiConfig(nil))
	}
}

func TestFilterGrokConfigDropsSkillsKeepsModel(t *testing.T) {
	src := []byte(`
model = "grok-4.6"

[skills]
paths = ["~/operator-skills"]

[compat.claude]
skills = true

[plugins]
enabled = true
`)
	got := string(FilterGrokConfig(src))
	if !strings.Contains(got, `model = "grok-4.6"`) {
		t.Fatalf("dropped model: %s", got)
	}
	if !strings.Contains(got, "[compat.claude]") || !strings.Contains(got, "skills = false") {
		t.Fatalf("missing forced compat: %s", got)
	}
	for _, banned := range []string{"operator-skills", "skills = true", "[plugins]"} {
		if strings.Contains(got, banned) {
			t.Fatalf("kept %q: %s", banned, got)
		}
	}
}

func TestPrepareCreatesBothHomes(t *testing.T) {
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

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
)

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
	if err := os.MkdirAll(filepath.Join(src, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "kimi")
	if err := PrepareKimi(dest, src); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dest, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(src, "credentials") {
		t.Fatalf("credentials link = %q", target)
	}
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatalf("skills dir: %v", err)
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

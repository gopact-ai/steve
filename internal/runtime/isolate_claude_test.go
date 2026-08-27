package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareClaudeLinksCredentialsAndFiltersSettings(t *testing.T) {
	user := t.TempDir()
	dest := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(filepath.Join(user, ".credentials.json"), []byte(`{"tok":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{
		"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1/v1", "ANTHROPIC_CUSTOM_HEADERS": "x-a: b"},
		"permissions": {"defaultMode": "auto"},
		"model": "opus",
		"hooks": {"PostToolUse": []}
	}`
	if err := os.WriteFile(filepath.Join(user, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareClaude(dest, user); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(dest, ".credentials.json"))
	if err != nil || link != filepath.Join(user, ".credentials.json") {
		t.Fatalf("credentials link = %q, %v; a copy would go stale on token refresh", link, err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("filtered settings are not JSON: %s", raw)
	}
	if len(got) != 1 || got["env"] == nil {
		t.Fatalf("filtered settings must keep env and drop the rest: %s", raw)
	}
	env := got["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1/v1" || env["ANTHROPIC_CUSTOM_HEADERS"] != "x-a: b" {
		t.Fatalf("env not carried over: %s", raw)
	}
}

func TestPrepareClaudeWithoutOperatorConfig(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "claude")
	if err := PrepareClaude(dest, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("phantom credentials link")
	}
	raw, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil || strings.TrimSpace(string(raw)) != "{}" {
		t.Fatalf("settings = %q, %v; want an empty object", raw, err)
	}
	if info, err := os.Stat(filepath.Join(dest, "skills")); err != nil || !info.IsDir() {
		t.Fatal("skills dir missing")
	}
}

func TestFilterClaudeSettingsBadJSON(t *testing.T) {
	if out := strings.TrimSpace(string(FilterClaudeSettings([]byte("not json")))); out != "{}" {
		t.Fatalf("bad json -> %q, want {}", out)
	}
}

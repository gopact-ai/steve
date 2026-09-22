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
		"effortLevel": "high",
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
		t.Fatalf("filtered settings must keep env and drop the operator's model choice, effort, permissions and hooks: %s", raw)
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

// The operator's model catalogue must reach the isolated harness, or the
// picker offers Claude Code's built-in lineup instead of the models the
// operator's endpoint actually serves (an operator whose relay serves
// Fable through modelPicker saw no Fable in the console).
func TestFilterClaudeSettingsKeepsModelCatalogue(t *testing.T) {
	settings := `{
		"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1"},
		"model": "claude-opus-5",
		"modelPicker": {"replaceBuiltInOptions": true, "options": [{"model": "claude-fable-5", "label": "Fable 5", "behavesAs": "claude-opus-5"}]},
		"availableModels": ["claude-fable-5", "claude-opus-5"],
		"modelOverrides": {"claude-opus-5": "arn:aws:bedrock:us-east-1:1:inference-profile/opus"},
		"modelSettings": {"claude-fable-5": {"effortLevel": "high"}},
		"permissions": {"defaultMode": "auto"}
	}`
	var got map[string]any
	if err := json.Unmarshal(FilterClaudeSettings([]byte(settings)), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"env", "modelPicker", "availableModels", "modelOverrides"} {
		if got[key] == nil {
			t.Errorf("%s dropped: the isolated harness cannot offer the operator's models without it", key)
		}
	}
	for _, key := range []string{"model", "modelSettings", "permissions"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s kept: the operator's own default model, effort and permissions are not the harness's", key)
		}
	}
	picker, _ := got["modelPicker"].(map[string]any)
	options, _ := picker["options"].([]any)
	if len(options) != 1 {
		t.Fatalf("modelPicker options = %v; the rows must arrive verbatim", got["modelPicker"])
	}
}

// A key present as JSON null must not be forwarded: Claude Code validates
// settings.json and a null catalogue is an error, not an absence.
func TestFilterClaudeSettingsDropsNullValues(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(FilterClaudeSettings([]byte(`{"env": null, "modelPicker": null, "availableModels": []}`)), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["env"]; ok {
		t.Errorf("null env kept: %v", got)
	}
	if _, ok := got["modelPicker"]; ok {
		t.Errorf("null modelPicker kept: %v", got)
	}
	if list, ok := got["availableModels"].([]any); !ok || len(list) != 0 {
		t.Errorf("empty availableModels must survive, it means \"only Default\": %v", got)
	}
}

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

func TestNativeRuntimeCombinesSelectedHistoryWithAdmittedAccessAndSkills(t *testing.T) {
	source, active, state := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(home, name, body string) {
		t.Helper()
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	transcript := "sessions/2026/09/14/rollout-selected.jsonl"
	write(source, transcript, `{"type":"session_meta","payload":{"id":"selected","cwd":"/work"}}`+"\n")
	write(source, "auth.json", "old-credentials")
	write(source, "skills/unselected/SKILL.md", "old skill")
	write(active, "auth.json", "admitted-credentials")
	write(active, "config.toml", "model_provider = \"approved\"\n[mcp_servers.unselected]\ncommand = \"must-not-start\"\n")
	write(active, "skills/approved/SKILL.md", "admitted skill")
	src := nativehistory.Source{Harness: "codex", Home: source}
	entries, err := nativehistory.List(t.Context(), src)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	ref, err := nativehistory.Snapshot(t.Context(), filepath.Join(state, "native-imports"), nativehistory.ImportRequest{CommandID: "import", Source: src, NativeID: "selected", Revision: entries[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := PrepareNativeHistory(t.Context(), state, "ns_selected", ref, harness.Config{Env: []string{"CODEX_HOME=" + active, "STEVE_PLUGIN_SKILLS_DIR=" + filepath.Join(active, "skills")}})
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(state, "native-runtimes/ns_selected/home")
	for name, want := range map[string]string{"auth.json": "admitted-credentials", "skills/approved/SKILL.md": "admitted skill", transcript: `"id":"selected"`} {
		raw, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !strings.Contains(string(raw), want) {
			t.Fatalf("runtime %s: %q %v", name, raw, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil || !strings.Contains(string(raw), "approved") || strings.Contains(string(raw), "must-not-start") {
		t.Fatalf("access filter: %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills/unselected")); !os.IsNotExist(err) {
		t.Fatal("old unselected skill imported")
	}
	if !strings.Contains(strings.Join(cfg.Env, "\n"), "STEVE_PLUGIN_SKILLS_DIR="+filepath.Join(dest, "skills")) {
		t.Fatal("plugin skill path was not isolated")
	}
	if _, err := PrepareNativeHistory(t.Context(), state, "ns_selected", ref, cfg); err == nil {
		t.Fatal("existing execution overwritten")
	}
}

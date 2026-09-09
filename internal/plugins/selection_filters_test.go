package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPresetSelectionFiltersArePinnedAndValidated(t *testing.T) {
	dir := fixtureDirectory(t)
	m := example(t, "github").Manifest
	if err := os.MkdirAll(filepath.Join(dir, "skills/extra"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills/extra/SKILL.md"), []byte("UNSELECTED_SKILL"), 0600); err != nil {
		t.Fatal(err)
	}
	m.Skills["extra"] = "skills/extra"
	m.MCP = nil
	m.Agents = nil
	m.Settings = nil
	writeManifest(t, dir, m)
	bundle, err := ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	if _, err := store.Install(t.Context(), InstallRequest{CommandID: "package", ExpectedDigest: bundle.Digest, Bundle: bundle, Source: Source{Kind: "bundle", Location: bundle.Digest}}); err != nil {
		t.Fatal(err)
	}
	deployment := Deployment{Installation: "tools", PackageID: m.ID, Digest: bundle.Digest, Projects: []string{"p"}}
	receipt, err := store.PrepareDeployment(t.Context(), deployment, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{Project: "p", Harness: "mock", Deployments: []string{receipt.Hash}, Filters: map[string]CapabilityFilter{"tools": {Skills: []string{"review"}}}}
	if _, err := store.Selection(selection); err != nil {
		t.Fatal(err)
	}
	text, err := store.RuntimeInstructions(selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) == 0 {
		t.Fatal("selected skill absent")
	}
	skillDir := filepath.Join(t.TempDir(), "skills")
	if _, err := store.MaterializeRuntimeSkills(t.Context(), selection, skillDir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(skillDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unselected skills materialized: %v %v", entries, err)
	}
	plain := selection.Clone()
	plain.Filters = nil
	a, _ := selection.Hash()
	b, _ := plain.Hash()
	if a == b {
		t.Fatal("selection filter not in runtime identity")
	}
	invalid := selection.Clone()
	invalid.Filters["tools"] = CapabilityFilter{Skills: []string{"unknown"}}
	if _, err := store.Selection(invalid); err == nil {
		t.Fatal("unknown filter accepted")
	}
	raw, _ := json.Marshal(selection)
	var restored Selection
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	hash, _ := restored.Hash()
	if hash != a {
		t.Fatal("filter identity changed on persistence")
	}
}

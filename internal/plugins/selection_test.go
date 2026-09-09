package plugins

import "testing"

func TestSessionSelectionPinsNodeProjectAndInstallationRevision(t *testing.T) {
	store, d := configuredDeployment(t)
	receipt, err := store.PrepareDeployment(t.Context(), d, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{Project: "p", Node: "worker", Harness: "codex", Deployments: []string{receipt.Hash}}
	if _, err := store.Selection(selection); err != nil {
		t.Fatal(err)
	}
	other := selection
	other.Project = "unrelated"
	if _, err := store.Selection(other); err == nil {
		t.Fatal("selection crossed project scope")
	}
	other = selection
	other.Node = "other"
	if _, err := store.Selection(other); err == nil {
		t.Fatal("selection crossed node scope")
	}
	other = selection
	other.Deployments = []string{receipt.Hash, receipt.Hash}
	if _, err := store.Selection(other); err == nil {
		t.Fatal("duplicate deployment accepted")
	}
	next := d
	next.Configuration = d.Configuration.Clone()
	next.Configuration.Values["endpoint"] = "https://another.invalid/mcp"
	second, err := store.PrepareDeployment(t.Context(), next, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	other = selection
	other.Deployments = []string{receipt.Hash, second.Hash}
	if _, err := store.Selection(other); err == nil {
		t.Fatal("mixed revisions of one installation")
	}
	ref := &RuntimeRef{ID: receipt.Hash, Selection: selection}
	copy := ref.Clone()
	copy.Selection.Deployments[0] = second.Hash
	if ref.Selection.Deployments[0] != receipt.Hash {
		t.Fatal("runtime reference aliases cloned selection")
	}
}

package plugins

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestRelocationRetainsOriginalVersionAndTargetCredentialReferences(t *testing.T) {
	store, d := configuredDeployment(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	library := &Library{Store: store, Ledger: book}
	bundle, err := store.Read(d.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.Add(t.Context(), "p", bundle); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.PrepareDeployment(t.Context(), d, Environment{})
	if err != nil {
		t.Fatal(err)
	}
	if err := library.RecordDeployment(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	target := SecretRef{Name: "other-node", Revision: strings.Repeat("a", 32)}
	items := map[string]Installation{"team": {PackageID: d.PackageID, Digest: strings.Repeat("b", 64), Enabled: false, Projects: []string{"p"}, Targets: map[string]Configuration{"worker": d.Configuration, "replacement": {Secrets: map[string]SecretRef{"token": target}}}}}
	source := RuntimeRef{ID: strings.Repeat("c", 64), Selection: Selection{Project: "p", Node: "worker", Harness: "mock", Deployments: []string{receipt.Hash}, Filters: map[string]CapabilityFilter{"team": {Skills: []string{}, MCP: []string{"team"}}}}}
	plan, err := library.PlanRelocation(t.Context(), items, source, "replacement", "mock")
	if err != nil {
		t.Fatal(err)
	}
	deployed := plan.Deployments[0]
	if deployed.Digest != d.Digest || deployed.Configuration.Values["endpoint"] != d.Configuration.Values["endpoint"] || deployed.Configuration.Secrets["token"] != target {
		t.Fatalf("recovery changed old version or copied source credential: %+v", deployed)
	}
	if plan.Selection.Includes("team", "skill", "conventions") || !plan.Selection.Includes("team", "mcp", "team") {
		t.Fatal("relocation dropped capability filter")
	}
	raw, err := json.Marshal(plan)
	if err != nil || strings.Contains(string(raw), "private-node-token") {
		t.Fatal("secret value entered recovery plan")
	}
	if err := plan.CheckScope(items); err != nil {
		t.Fatal(err)
	}
	delete(items["team"].Targets, "replacement")
	if err := plan.CheckScope(items); err == nil {
		t.Fatal("saved recovery plan restored revoked target scope")
	}
}

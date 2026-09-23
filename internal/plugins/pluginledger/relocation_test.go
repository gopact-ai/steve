package pluginledger

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plugins"
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
	receipt, err := store.PrepareDeployment(t.Context(), d, plugins.Environment{})
	if err != nil {
		t.Fatal(err)
	}
	if err := library.RecordDeployment(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	target := plugins.SecretRef{Name: "other-node", Revision: strings.Repeat("a", 32)}
	items := map[string]plugins.Installation{"team": {PackageID: d.PackageID, Digest: strings.Repeat("b", 64), Enabled: false, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"worker": d.Configuration, "replacement": {Secrets: map[string]plugins.SecretRef{"token": target}}}}}
	source := plugins.RuntimeRef{ID: strings.Repeat("c", 64), Selection: plugins.Selection{Project: "p", Node: "worker", Harness: "mock", Deployments: []string{receipt.Hash}, Filters: map[string]plugins.CapabilityFilter{"team": {Skills: []string{}, MCP: []string{"team"}}}}}
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

func configuredDeployment(t *testing.T) (*plugins.Store, plugins.Deployment) {
	t.Helper()
	bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "..", "examples", "plugins", "team-tools"))
	if err != nil {
		t.Fatal(err)
	}
	store := &plugins.Store{Dir: filepath.Join(t.TempDir(), "plugins")}
	if _, err := store.Install(t.Context(), plugins.InstallRequest{CommandID: "package", ExpectedDigest: bundle.Digest, Bundle: bundle, Source: plugins.Source{Kind: "directory", Location: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	secret, err := store.PutSecret(t.Context(), "team-token", "private-node-token")
	if err != nil {
		t.Fatal(err)
	}
	d := plugins.Deployment{Installation: "team", PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Node: "worker", Projects: []string{"p"}, Configuration: plugins.Configuration{Values: map[string]string{"endpoint": "https://team.example.invalid/mcp"}, Secrets: map[string]plugins.SecretRef{"token": secret.Reference}}}
	return store, d
}

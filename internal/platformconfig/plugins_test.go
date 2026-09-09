package platformconfig

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPluginConfigurationSurvivesCoordinatorChangeWithoutAliasing(t *testing.T) {
	cfg := initialConfig()
	cfg.Plugins = map[string]plugins.Installation{"github": {PackageID: "gopact/github", Digest: strings.Repeat("a", 64), Projects: []string{"work"}, Targets: map[string]plugins.Configuration{"": {Values: map[string]string{"endpoint": "https://example.invalid"}, Secrets: map[string]plugins.SecretRef{"token": {Name: "github", Revision: strings.Repeat("b", 32)}}}}}}
	declaration, err := FromLocal(cfg, LocalNode{ID: "first", Config: config.Node{Addr: "worker", Token: "node"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := declaration.Plugins["github"].Targets[""]; exists {
		t.Fatal("plugin target stayed coordinator-relative")
	}
	other := &config.Config{}
	if err := declaration.Apply(other); err != nil {
		t.Fatal(err)
	}
	item := other.Plugins["github"]
	item.Projects[0] = "changed"
	item.Targets["first"].Values["endpoint"] = "changed"
	if declaration.Plugins["github"].Projects[0] != "work" || declaration.Plugins["github"].Targets["first"].Values["endpoint"] != "https://example.invalid" {
		t.Fatal("applied configuration aliases authority")
	}
}

func TestPluginLocalTargetCannotOverwritePhysicalTarget(t *testing.T) {
	cfg := initialConfig()
	cfg.Nodes = map[string]config.Node{"first": {Addr: "worker", Token: "node"}}
	cfg.Plugins = map[string]plugins.Installation{"tools": {
		PackageID: "example/team-tools", Digest: strings.Repeat("a", 64), Projects: []string{"work"},
		Targets: map[string]plugins.Configuration{"": {}, "first": {Values: map[string]string{"endpoint": "different"}}},
	}}
	if _, err := FromLocal(cfg, LocalNode{ID: "first", Config: cfg.Nodes["first"]}); err == nil {
		t.Fatal("ambiguous local deployment was overwritten by map iteration")
	}
}

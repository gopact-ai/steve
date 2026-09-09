package config

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPluginScopesRequireKnownProjectsAndSuitableMachines(t *testing.T) {
	cfg := &Config{Projects: map[string]Project{"work": {Level: "restricted"}}, Nodes: map[string]Node{"worker": {Level: "restricted"}}, Plugins: map[string]plugins.Installation{"github": {PackageID: "gopact/github", Digest: strings.Repeat("a", 64), Projects: []string{"work"}, Targets: map[string]plugins.Configuration{"worker": {}}}}}
	if err := cfg.ValidatePlugins(); err != nil {
		t.Fatal(err)
	}
	cfg.Nodes["worker"] = Node{Level: "public"}
	if err := cfg.ValidatePlugins(); err == nil {
		t.Fatal("private package scope sent to public node")
	}
	cfg.Nodes["worker"] = Node{Level: "restricted"}
	delete(cfg.Projects, "work")
	if err := cfg.ValidatePlugins(); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestPluginScopesRejectDuplicatePackageActivation(t *testing.T) {
	item := plugins.Installation{PackageID: "test/package", Digest: strings.Repeat("a", 64), Enabled: true, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}
	cfg := &Config{Projects: map[string]Project{"p": {}}, Plugins: map[string]plugins.Installation{"one": item, "two": item}}
	if err := cfg.ValidatePlugins(); err == nil {
		t.Fatal("duplicate runtime capability names passed configuration review")
	}
	item.Enabled = false
	cfg.Plugins["two"] = item
	if err := cfg.ValidatePlugins(); err != nil {
		t.Fatalf("inactive candidate cannot coexist: %v", err)
	}
}

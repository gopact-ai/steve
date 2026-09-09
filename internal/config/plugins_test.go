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

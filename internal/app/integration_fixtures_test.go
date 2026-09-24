package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func ChannelValue[T any](v T) *T { return &v }

func ChannelsAdminFixture(t *testing.T) (*adminsvc.Service, consoleapi.ChannelsService) {
	t.Helper()
	a := agentAdminFixture(t)
	configOf(a).Gateway.OwnerID, configOf(a).Gateway.DefaultChannel = "console-owner", "feishu"
	configOf(a).Gateway.StatePath = filepath.Join(t.TempDir(), "state.json")
	configOf(a).Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	configOf(a).Feishu = config.Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner"}
	if err := config.Save(a.Path, configOf(a)); err != nil {
		t.Fatal(err)
	}
	return a, adminsvc.NewChannels(a, configOf(a))
}

func AssertNoChannelSecrets(t *testing.T, view consoleapi.ChannelsView) {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), `"app_secret":`) {
		t.Fatal("channels response exposed a secret")
	}
}

func agentAdminFixture(t *testing.T) *adminsvc.Service {
	t.Helper()
	cfg := &config.Config{
		Harnesses: map[string]config.Harness{"mock": {Command: "mock"}},
		Agents: map[string]config.Agent{
			"primary": {Harness: "mock", Default: true, Aliases: []string{"main"}},
			"worker":  {Harness: "mock", Model: "before", Options: map[string]string{"effort": "high"}, Skills: []string{"skill"}},
		},
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	a := &adminsvc.Service{ConfigStore: adminsvc.NewConfigStore(cfg), Path: path, Catalog: catalog}
	fixtureConfigs.Store(a, cfg)
	t.Cleanup(func() { fixtureConfigs.Delete(a) })
	return a
}

// fixtureConfigs holds the configuration agentAdminFixture built for each
// service it returned.
var fixtureConfigs sync.Map

// configOf is the configuration agentAdminFixture built for a. Tests
// change it only before a is in use and read it only while nothing else
// changes it.
func configOf(a *adminsvc.Service) *config.Config {
	cfg, _ := fixtureConfigs.Load(a)
	return cfg.(*config.Config)
}

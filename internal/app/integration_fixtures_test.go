package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func ChannelValue[T any](v T) *T { return &v }

func ChannelsAdminFixture(t *testing.T) (*adminsvc.Service, consoleapi.ChannelsService) {
	t.Helper()
	a := agentAdminFixture(t)
	a.Cfg.Gateway.OwnerID, a.Cfg.Gateway.DefaultChannel = "console-owner", "feishu"
	a.Cfg.Gateway.StatePath = filepath.Join(t.TempDir(), "state.json")
	a.Cfg.Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	a.Cfg.Feishu = config.Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner"}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	return a, adminsvc.NewChannels(a, a.Cfg)
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
	return &adminsvc.Service{Cfg: cfg, Path: path, Catalog: catalog}
}

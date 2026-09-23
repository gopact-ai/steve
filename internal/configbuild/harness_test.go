package configbuild

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestRemoteAgentDoesNotRequireCoordinatorCommandOrCredentials(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `{"harnesses":{},"agents":{"remote":{"harness":"remote-only","node":"worker","default":true}},"nodes":{"worker":{"addr":"127.0.0.1:7701","token":"test-token"}},"projects":{"workspace":{"home":{"path":"/tmp/steve-remote-enrollment"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harnesses) != 0 || cfg.Agents["remote"].Node != "worker" {
		t.Fatalf("remote config was copied locally: %+v", cfg.Harnesses)
	}
	manager, err := HarnessManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.Stop()
}

func TestRemoteAgentStillRequiresKnownNode(t *testing.T) {
	if _, err := config.Load(writeConfig(t, `{"agents":{"remote":{"harness":"remote-only","node":"missing","default":true}},"projects":{"workspace":{"home":{"path":"/tmp/steve-remote-enrollment"}}}}`)); err == nil {
		t.Fatal("remote Agent accepted an unknown node")
	}
}

func TestFirstLaunchBuildsAnEmptyHarnessManager(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `{
		"agents": {}, "harnesses": {},
		"projects": {"workspace": {"home": {"path": "/tmp/steve-desktop-workspace"}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := HarnessManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.Stop()
}

func TestProjectListFollowsMigratedAgentWorkspaces(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `{
		"agents": {
			"codex": {"harness": "codex", "workspace": "/tmp/steve-codex", "default": true},
			"lab": {"harness": "codex", "node": "host-3", "workspace": "/srv/lab"}
		},
		"nodes": {"host-3": {"addr": "10.0.0.3:7701", "token": "t"}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	list := ProjectList(cfg)
	if len(list) != 2 || list[0].ID != "codex" || list[1].ID != "lab" {
		t.Fatalf("project list = %+v", list)
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

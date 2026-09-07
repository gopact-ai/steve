package config

import "testing"

func TestRemoteAgentDoesNotRequireCoordinatorCommandOrCredentials(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"harnesses":{},"agents":{"remote":{"harness":"remote-only","node":"worker","default":true}},"nodes":{"worker":{"addr":"127.0.0.1:7701","token":"test-token"}},"projects":{"workspace":{"home":{"path":"/tmp/steve-remote-enrollment"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harnesses) != 0 || cfg.Agents["remote"].Node != "worker" {
		t.Fatalf("remote config was copied locally: %+v", cfg.Harnesses)
	}
	manager, err := cfg.HarnessManager()
	if err != nil {
		t.Fatal(err)
	}
	manager.Stop()
}

func TestRemoteAgentStillRequiresKnownNode(t *testing.T) {
	if _, err := Load(writeConfig(t, `{"agents":{"remote":{"harness":"remote-only","node":"missing","default":true}},"projects":{"workspace":{"home":{"path":"/tmp/steve-remote-enrollment"}}}}`)); err == nil {
		t.Fatal("remote Agent accepted an unknown node")
	}
}

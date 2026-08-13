package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadJSON(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-test", "default": true}},
		"harnesses": {"codex": {"command": "mockagent"}},
		"feishu": {"app_id": "app", "app_secret": "secret", "allowed_senders": ["ou_user"]},
		"gateway": {"prompt_timeout": "30s"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Gateway.PromptTimeout) != 30*time.Second {
		t.Fatalf("unexpected prompt timeout: %s", time.Duration(cfg.Gateway.PromptTimeout))
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{"agents":{"codex":{"harness":"codex","workspace":"/tmp/steve-test","default":true,"harnses":"typo"}}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRejectsUnknownHarness(t *testing.T) {
	path := writeConfig(t, `{
		"agents":{"codex":{"harness":"missing","workspace":"/tmp/steve-test","default":true}},
		"harnesses":{"codex":{"command":"mockagent"}}
	}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("expected unknown harness error, got %v", err)
	}
}

func TestLoadRejectsMissingWorkspace(t *testing.T) {
	path := writeConfig(t, `{
		"agents":{"codex":{"harness":"codex","default":true}},
		"harnesses":{"codex":{"command":"mockagent"}}
	}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("expected workspace error, got %v", err)
	}
}

func TestStarterDefaultsToDeny(t *testing.T) {
	for id, harness := range Starter("app", "secret", "ou_user").Harnesses {
		if harness.Permission != "deny" {
			t.Fatalf("harness %q permission = %q, want deny", id, harness.Permission)
		}
	}
}

func TestValidateFeishuCredentials(t *testing.T) {
	if err := (Feishu{}).Validate(); err == nil {
		t.Fatal("expected missing Feishu credentials error")
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret"}).Validate(); err != nil {
		t.Fatalf("pairing default should allow empty senders: %v", err)
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret", DMPolicy: DMPolicyAllowlist}).Validate(); err == nil {
		t.Fatal("expected missing sender allowlist error")
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret", Domain: "slack"}).Validate(); err == nil {
		t.Fatal("expected invalid domain error")
	}
}

func TestLoadDerivesHomePathAndTrimsOwner(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-test", "default": true}},
		"harnesses": {"codex": {"command": "mockagent"}},
		"feishu": {"app_id": "app", "app_secret": "secret", "owner_open_id": "  ou_owner  "},
		"gateway": {"state_path": "/tmp/steve-state.json"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.OwnerOpenID != "ou_owner" {
		t.Fatalf("owner = %q", cfg.Feishu.OwnerOpenID)
	}
	wantHome := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "home")
	if cfg.Gateway.HomePath != wantHome {
		t.Fatalf("home = %q, want %q", cfg.Gateway.HomePath, wantHome)
	}
}

func TestStarterSaveOmitsHomePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, StarterFeishu(Feishu{AppID: "app", AppSecret: "secret", OwnerOpenID: "ou_me"})); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "home_path") {
		t.Fatalf("starter persisted home_path: %s", raw)
	}
	if !strings.Contains(string(raw), "ou_me") {
		t.Fatalf("owner missing: %s", raw)
	}
}

func TestLoadAppliesFeishuDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-test", "default": true}},
		"harnesses": {"codex": {"command": "mockagent"}},
		"feishu": {"app_id": "app", "app_secret": "secret"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.Domain != DomainFeishu || cfg.Feishu.DMPolicy != DMPolicyPairing || cfg.Feishu.GroupPolicy != GroupPolicyAllowlist {
		t.Fatalf("unexpected defaults: %#v", cfg.Feishu)
	}
}

func TestLoadKeepsAllowlistWhenSendersExist(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-test", "default": true}},
		"harnesses": {"codex": {"command": "mockagent"}},
		"feishu": {"app_id": "app", "app_secret": "secret", "allowed_senders": ["ou_user"]}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.DMPolicy != DMPolicyAllowlist {
		t.Fatalf("existing sender list should stay allowlist, got %q", cfg.Feishu.DMPolicy)
	}
}

func TestSaveCreatesPrivateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Starter("app", "secret", "ou_user")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saved config cannot be loaded: %v", err)
	}
	if err := Save(path, Starter("other", "secret", "ou_user")); err == nil {
		t.Fatal("Save overwrote an existing config")
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

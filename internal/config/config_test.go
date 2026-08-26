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

func TestStarterDefaultsToRead(t *testing.T) {
	for id, harness := range Starter("app", "secret", "ou_user").Harnesses {
		if harness.Permission != PermissionRead {
			t.Fatalf("harness %q permission = %q, want read", id, harness.Permission)
		}
	}
}

func TestStarterIncludesGrokAndKimi(t *testing.T) {
	cfg := Starter("app", "secret", "ou_user")
	for _, id := range []string{"grok", "kimi"} {
		if _, ok := cfg.Agents[id]; !ok {
			t.Fatalf("missing agent %q", id)
		}
		if _, ok := cfg.Harnesses[id]; !ok {
			t.Fatalf("missing harness %q", id)
		}
	}
	if cfg.Agents["grok"].Harness != "grok" || cfg.Harnesses["grok"].Command != "grok" {
		t.Fatalf("grok = %#v %#v", cfg.Agents["grok"], cfg.Harnesses["grok"])
	}
	if cfg.Agents["kimi"].Harness != "kimi" || cfg.Harnesses["kimi"].Command != "kimi" {
		t.Fatalf("kimi = %#v %#v", cfg.Agents["kimi"], cfg.Harnesses["kimi"])
	}
	if got := cfg.Harnesses["grok"].Args; len(got) != 3 || got[0] != "agent" || got[2] != "stdio" {
		t.Fatalf("grok args = %#v", got)
	}
	if got := cfg.Harnesses["kimi"].Args; len(got) != 1 || got[0] != "acp" {
		t.Fatalf("kimi args = %#v", got)
	}
}

func TestValidateFeishuCredentials(t *testing.T) {
	if err := (Feishu{}).Validate(); err == nil {
		t.Fatal("expected missing Feishu credentials error")
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret"}).Validate(); err != nil {
		t.Fatalf("empty allowlist should be open: %v", err)
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret", AllowedSenders: []string{" "}}).Validate(); err == nil {
		t.Fatal("expected empty sender id error")
	}
	if err := (Feishu{AppID: "app", AppSecret: "secret", BlockedSenders: []string{""}}).Validate(); err == nil {
		t.Fatal("expected empty blocked id error")
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
	if cfg.Feishu.Domain != DomainFeishu || cfg.Feishu.GroupPolicy != GroupPolicyOpen || cfg.Feishu.DMPolicy != "" {
		t.Fatalf("unexpected defaults: %#v", cfg.Feishu)
	}
}

func TestLoadKeepsSenderLists(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-test", "default": true}},
		"harnesses": {"codex": {"command": "mockagent"}},
		"feishu": {"app_id": "app", "app_secret": "secret", "allowed_senders": ["ou_user"], "blocked_senders": ["ou_spam"]}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.GroupPolicy != GroupPolicyOpen {
		t.Fatalf("group policy = %q", cfg.Feishu.GroupPolicy)
	}
	if len(cfg.Feishu.AllowedSenders) != 1 || cfg.Feishu.AllowedSenders[0] != "ou_user" {
		t.Fatalf("allowed = %#v", cfg.Feishu.AllowedSenders)
	}
	if len(cfg.Feishu.BlockedSenders) != 1 || cfg.Feishu.BlockedSenders[0] != "ou_spam" {
		t.Fatalf("blocked = %#v", cfg.Feishu.BlockedSenders)
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
	if err := Save(path, Starter("other", "secret", "ou_user")); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Feishu.AppID != "other" {
		t.Fatalf("overwrite did not persist: %#v", loaded.Feishu)
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

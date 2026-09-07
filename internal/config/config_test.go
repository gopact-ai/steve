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

func TestLoadMigratesAgentWorkspacesIntoProjects(t *testing.T) {
	path := writeConfig(t, `{
		"agents": {
			"codex": {"harness": "codex", "workspace": "/tmp/steve-codex", "default": true},
			"lab": {"harness": "codex", "node": "host-3", "workspace": "/srv/lab"}
		},
		"nodes": {"host-3": {"addr": "10.0.0.3:7701", "token": "t"}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Projects["codex"].Home; got.Node != "" || got.Path != "/tmp/steve-codex" {
		t.Fatalf("hub project home = %+v", got)
	}
	if got := cfg.Projects["lab"].Home; got.Node != "host-3" || got.Path != "/srv/lab" {
		t.Fatalf("remote project home = %+v (must stay the node's own path)", got)
	}
	if cfg.Gateway.DefaultProject != "codex" {
		t.Fatalf("default project = %q, want the default agent's", cfg.Gateway.DefaultProject)
	}
	if len(cfg.Migrated) != 1 || !strings.Contains(cfg.Migrated[0], "projects") {
		t.Fatalf("migration note = %v", cfg.Migrated)
	}
	for id, a := range cfg.Agents {
		if a.LegacyWorkspace != "" {
			t.Fatalf("agent %s still carries a workspace after migration", id)
		}
	}
	list := cfg.ProjectList()
	if len(list) != 2 || list[0].ID != "codex" || list[1].ID != "lab" {
		t.Fatalf("project list = %+v", list)
	}
}

func TestLoadRefusesBothLayoutsAndReservedName(t *testing.T) {
	both := writeConfig(t, `{
		"projects": {"p": {"home": {"path": "/tmp/p"}}},
		"agents": {"codex": {"harness": "codex", "workspace": "/tmp/steve-codex", "default": true}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`)
	if _, err := Load(both); err == nil || !strings.Contains(err.Error(), "no longer read") {
		t.Fatalf("both layouts = %v", err)
	}
	reserved := writeConfig(t, `{
		"projects": {"home": {"home": {"path": "/tmp/p"}}},
		"agents": {"codex": {"harness": "codex", "default": true}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`)
	if _, err := Load(reserved); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved name = %v", err)
	}
	orphan := writeConfig(t, `{
		"projects": {"p": {"home": {"node": "ghost", "path": "/tmp/p"}}},
		"agents": {"codex": {"harness": "codex", "default": true}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`)
	if _, err := Load(orphan); err == nil || !strings.Contains(err.Error(), "nodes{}") {
		t.Fatalf("unknown home node = %v", err)
	}
	two := writeConfig(t, `{
		"projects": {"a": {"home": {"path": "/tmp/a"}}, "b": {"home": {"path": "/tmp/b"}}},
		"agents": {"codex": {"harness": "codex", "default": true}},
		"harnesses": {"codex": {"command": "true"}},
		"feishu": {"app_id": "cli", "app_secret": "s"}
	}`)
	cfg, err := Load(two)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.DefaultProject != "" {
		t.Fatalf("two projects must not pick a default silently, got %q", cfg.Gateway.DefaultProject)
	}
}

// A harness names an adapter or a command. Both is a contradiction about
// what runs; neither leaves nothing to run.
func TestHarnessTakesAnAdapterOrACommandButNotBoth(t *testing.T) {
	for _, c := range []struct {
		name    string
		harness string
		wants   string
	}{
		{"both", `{"adapter":"codex-acp","command":"/usr/bin/codex-acp"}`, "pick one"},
		{"neither", `{"permission":"read"}`, "needs an adapter or a command"},
		{"unknown adapter", `{"adapter":"nope-acp"}`, "is not one of"},
		{"adapter with args", `{"adapter":"codex-acp","args":["-y"]}`, "takes no args"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := writeAndLoad(t, `{
				"agents":{"codex":{"harness":"codex","default":true}},
				"harnesses":{"codex":`+c.harness+`},
				"projects":{"work":{"home":{"path":"/srv/work"}}},
				"feishu":{"app_id":"cli_x","app_secret":"s"},
				"gateway":{"state_path":"/tmp/steve/state.json"}}`)
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("error = %v, want it to mention %q", err, c.wants)
			}
		})
	}
}

// An adapter-backed harness loads without a command: the command arrives
// when the adapter is fetched, and loading a file must not need a network.
func TestAnAdapterHarnessLoadsWithoutACommand(t *testing.T) {
	cfg, err := writeAndLoad(t, `{
		"agents":{"codex":{"harness":"codex","default":true}},
		"harnesses":{"codex":{"adapter":"codex-acp"}},
		"projects":{"work":{"home":{"path":"/srv/work"}}},
		"feishu":{"app_id":"cli_x","app_secret":"s"},
		"gateway":{"state_path":"/tmp/steve/state.json"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Harnesses["codex"]; got.Adapter != "codex-acp" || got.Command != "" {
		t.Fatalf("harness = %#v", got)
	}
	if dir := cfg.AdapterDir(); !strings.HasSuffix(dir, "/adapters") {
		t.Fatalf("adapter dir = %q", dir)
	}
}

func writeAndLoad(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestDefaultMessageChannel(t *testing.T) {
	for _, name := range []string{"", "console"} {
		raw := `{"agents":{"codex":{"harness":"codex","workspace":"/tmp/steve-test","default":true}},"harnesses":{"codex":{"command":"mockagent"}},"feishu":{"app_id":"app","app_secret":"secret"},"gateway":{"default_channel":"` + name + `"}}`
		cfg, err := Load(writeConfig(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		want := name
		if want == "" {
			want = "feishu"
		}
		if cfg.Gateway.DefaultChannel != want {
			t.Fatalf("channel=%s want=%s", cfg.Gateway.DefaultChannel, want)
		}
	}
}

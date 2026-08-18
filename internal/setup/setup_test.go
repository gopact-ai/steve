package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
)

func TestRunScriptedOpenGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	var out bytes.Buffer
	cfg, err := Run(t.Context(), Flags{
		ConfigPath: path,
		AppID:      "cli_app",
		SecretEnv:  "FEISHU_APP_SECRET",
		Domain:     config.DomainLark,
	}, Options{
		Out: &out,
		Env: func(string) string { return "super-secret" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot", Name: "Steve"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) {
			return "ou_owner", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.Domain != config.DomainLark || cfg.Feishu.GroupPolicy != config.GroupPolicyOpen {
		t.Fatalf("unexpected config: %#v", cfg.Feishu)
	}
	if cfg.Feishu.OwnerOpenID != "ou_owner" {
		t.Fatalf("owner_open_id = %q", cfg.Feishu.OwnerOpenID)
	}
	if len(cfg.Feishu.AllowedSenders) != 0 {
		t.Fatalf("group allowlist should stay empty: %#v", cfg.Feishu.AllowedSenders)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Feishu.OwnerOpenID != "ou_owner" {
		t.Fatalf("saved owner_open_id = %q", loaded.Feishu.OwnerOpenID)
	}
	en := i18n.New(i18n.LocaleEN)
	if !strings.Contains(out.String(), en.T(i18n.SetupEditHome)) {
		t.Fatalf("lark setup did not use english: %s", out.String())
	}
	if _, err := os.Stat(home.DefaultPath()); !os.IsNotExist(err) {
		t.Fatal("setup wrote home files")
	}
	if strings.Contains(out.String(), "What should I call you") || strings.Contains(out.String(), "SOUL.md") {
		t.Fatalf("setup asked for identity: %s", out.String())
	}
}

func TestRunAllowlistEmptyIsOpen(t *testing.T) {
	cfg, err := Run(t.Context(), Flags{
		ConfigPath:  filepath.Join(t.TempDir(), "config.json"),
		AppID:       "cli_app",
		GroupPolicy: config.GroupPolicyAllowlist,
	}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "secret" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.GroupPolicy != config.GroupPolicyAllowlist || len(cfg.Feishu.AllowedSenders) != 0 {
		t.Fatalf("empty allowlist should remain valid: %#v", cfg.Feishu)
	}
}

func TestRunInteractiveCollectsFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	in := strings.NewReader("2\ncli_prompt\n2\n3\n")
	var out bytes.Buffer
	cfg, err := Run(t.Context(), Flags{ConfigPath: path}, Options{
		In:          in,
		Out:         &out,
		Interactive: true,
		Env:         func(string) string { return "" },
		ReadSecret:  func() (string, error) { return "typed-secret", nil },
		Probe: func(_ context.Context, appID, secret, domain string) (feishu.Identity, error) {
			if appID != "cli_prompt" || secret != "typed-secret" || domain != config.DomainLark {
				t.Fatalf("probe args = %s %s %s", appID, secret, domain)
			}
			return feishu.Identity{OpenID: "ou_bot", Name: "Bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.GroupPolicy != config.GroupPolicyDisabled {
		t.Fatalf("unexpected interactive config: %#v", cfg.Feishu)
	}
	if len(cfg.Feishu.AllowedSenders) != 0 {
		t.Fatalf("senders = %#v", cfg.Feishu.AllowedSenders)
	}
}

func TestRunCreateAppDeviceFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	var out bytes.Buffer
	cfg, err := Run(t.Context(), Flags{ConfigPath: path, CreateApp: true}, Options{
		Out: &out,
		Env: func(string) string { return "" },
		Register: func(context.Context, feishu.RegisterOptions) (feishu.CreatedApp, error) {
			return feishu.CreatedApp{
				AppID: "cli_created", AppSecret: "sec_created",
				Domain: config.DomainLark, OpenID: "ou_scan",
			}, nil
		},
		Probe: func(_ context.Context, appID, secret, domain string) (feishu.Identity, error) {
			if appID != "cli_created" || secret != "sec_created" || domain != config.DomainLark {
				t.Fatalf("probe args = %s %s %s", appID, secret, domain)
			}
			return feishu.Identity{OpenID: "ou_bot", Name: "Steve"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) {
			t.Fatal("owner lookup should be skipped when scan returns open_id")
			return "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.AppID != "cli_created" || cfg.Feishu.Domain != config.DomainLark {
		t.Fatalf("unexpected config: %#v", cfg.Feishu)
	}
	if cfg.Feishu.OwnerOpenID != "ou_scan" {
		t.Fatalf("scan open_id was not stored as owner: %#v", cfg.Feishu)
	}
	if len(cfg.Feishu.AllowedSenders) != 0 {
		t.Fatalf("owner must not be copied into group allowlist: %#v", cfg.Feishu.AllowedSenders)
	}
	if strings.Contains(out.String(), "sec_created") {
		t.Fatal("app secret was printed")
	}
}

func TestRunInteractiveDefaultsToCreateApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	in := strings.NewReader("\n\n\n")
	cfg, err := Run(t.Context(), Flags{ConfigPath: path}, Options{
		In:          in,
		Out:         ioDiscard(),
		Interactive: true,
		Env:         func(string) string { return "" },
		Register: func(context.Context, feishu.RegisterOptions) (feishu.CreatedApp, error) {
			return feishu.CreatedApp{AppID: "cli_new", AppSecret: "sec_new", Domain: config.DomainFeishu, OpenID: "ou_me"}, nil
		},
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.AppID != "cli_new" || cfg.Feishu.GroupPolicy != config.GroupPolicyOpen {
		t.Fatalf("unexpected config: %#v", cfg.Feishu)
	}
}

func TestRunOverwritesExistingConfigAndKeepsAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Starter("old-app", "old-secret", "ou_old")
	old.Agents["extra"] = config.Agent{Aliases: []string{"extra"}, Harness: "codex", Workspace: "/tmp/extra"}
	if err := config.Save(path, old); err != nil {
		t.Fatal(err)
	}
	_, err := Run(t.Context(), Flags{
		ConfigPath:  path,
		AppID:       "new-app",
		SecretEnv:   "FEISHU_APP_SECRET",
		Domain:      config.DomainFeishu,
		GroupPolicy: config.GroupPolicyAllowlist,
	}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "new-secret" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) {
			return "ou_new", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Feishu.AppID != "new-app" || loaded.Feishu.OwnerOpenID != "ou_new" {
		t.Fatalf("feishu not updated: %#v", loaded.Feishu)
	}
	if _, ok := loaded.Agents["extra"]; !ok {
		t.Fatalf("custom agent dropped: %#v", loaded.Agents)
	}
}

func TestRunAddsMissingGrokAndKimiAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Starter("old-app", "old-secret", "ou_old")
	delete(old.Agents, "grok")
	delete(old.Agents, "kimi")
	delete(old.Harnesses, "grok")
	delete(old.Harnesses, "kimi")
	if err := config.Save(path, old); err != nil {
		t.Fatal(err)
	}
	_, err := Run(t.Context(), Flags{
		ConfigPath:  path,
		AppID:       "new-app",
		SecretEnv:   "FEISHU_APP_SECRET",
		Domain:      config.DomainFeishu,
		GroupPolicy: config.GroupPolicyAllowlist,
	}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "new-secret" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) {
			return "ou_new", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"grok", "kimi"} {
		if _, ok := loaded.Agents[id]; !ok {
			t.Fatalf("missing agent %s: %#v", id, loaded.Agents)
		}
		if _, ok := loaded.Harnesses[id]; !ok {
			t.Fatalf("missing harness %s: %#v", id, loaded.Harnesses)
		}
	}
}

func TestRunReusesCachedConfigNonInteractive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Starter("cli_cached", "cached-secret", "ou_cached")
	old.Feishu.Domain = config.DomainFeishu
	if err := config.Save(path, old); err != nil {
		t.Fatal(err)
	}
	registered := false
	cfg, err := Run(t.Context(), Flags{ConfigPath: path}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "" },
		Register: func(context.Context, feishu.RegisterOptions) (feishu.CreatedApp, error) {
			registered = true
			return feishu.CreatedApp{}, errors.New("should not create app")
		},
		Probe: func(_ context.Context, appID, secret, domain string) (feishu.Identity, error) {
			if appID != "cli_cached" || secret != "cached-secret" {
				t.Fatalf("probe = %s %s %s", appID, secret, domain)
			}
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "ou_cached", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered {
		t.Fatal("reused setup created a new app")
	}
	if cfg.Feishu.AppID != "cli_cached" || cfg.Feishu.AppSecret != "cached-secret" {
		t.Fatalf("cached creds lost: %#v", cfg.Feishu)
	}
}

func TestRunInteractiveKeepsCachedSetup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Starter("cli_cached", "cached-secret", "ou_cached")
	if err := config.Save(path, old); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg, err := Run(t.Context(), Flags{ConfigPath: path}, Options{
		In:          strings.NewReader("\n"),
		Out:         &out,
		Interactive: true,
		Env:         func(string) string { return "" },
		Register: func(context.Context, feishu.RegisterOptions) (feishu.CreatedApp, error) {
			t.Fatal("keep should not create an app")
			return feishu.CreatedApp{}, nil
		},
		ReadSecret: func() (string, error) {
			t.Fatal("keep should not ask for a secret")
			return "", nil
		},
		Probe: func(_ context.Context, appID, secret, _ string) (feishu.Identity, error) {
			if appID != "cli_cached" || secret != "cached-secret" {
				t.Fatalf("probe = %s %s", appID, secret)
			}
			return feishu.Identity{OpenID: "ou_bot", Name: "Steve"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "ou_cached", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.AppID != "cli_cached" {
		t.Fatalf("app = %q", cfg.Feishu.AppID)
	}
	if !strings.Contains(out.String(), i18n.New(i18n.LocaleZH).T(i18n.SetupUsingCached)) {
		t.Fatalf("missing cached notice: %s", out.String())
	}
}

func TestRunInteractiveChangesAccessOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	old := config.Starter("cli_cached", "cached-secret", "ou_cached")
	old.Feishu.GroupPolicy = config.GroupPolicyAllowlist
	if err := config.Save(path, old); err != nil {
		t.Fatal(err)
	}
	cfg, err := Run(t.Context(), Flags{ConfigPath: path}, Options{
		In:          strings.NewReader("3\n1\n"),
		Out:         ioDiscard(),
		Interactive: true,
		Env:         func(string) string { return "" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "ou_cached", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Feishu.AppID != "cli_cached" || cfg.Feishu.AppSecret != "cached-secret" {
		t.Fatalf("creds changed: %#v", cfg.Feishu)
	}
	if cfg.Feishu.GroupPolicy != config.GroupPolicyOpen || len(cfg.Feishu.AllowedSenders) != 0 {
		t.Fatalf("access = %#v", cfg.Feishu)
	}
}

func TestRunRejectsCreateAppWithExistingCredentials(t *testing.T) {
	_, err := Run(t.Context(), Flags{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		AppID:      "cli_app",
		CreateApp:  true,
	}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "secret" },
	})
	if err == nil || !strings.Contains(err.Error(), "-create-app") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

type discard struct{}

func ioDiscard() *discard                    { return &discard{} }
func (*discard) Write(p []byte) (int, error) { return len(p), nil }

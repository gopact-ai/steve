package setup

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
)

func TestRunScriptedPairing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	var out bytes.Buffer
	cfg, err := Run(t.Context(), Flags{
		ConfigPath: path,
		AppID:      "cli_app",
		SecretEnv:  "FEISHU_APP_SECRET",
		DMPolicy:   config.DMPolicyPairing,
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
	if cfg.Feishu.Domain != config.DomainLark || cfg.Feishu.DMPolicy != config.DMPolicyPairing {
		t.Fatalf("unexpected config: %#v", cfg.Feishu)
	}
	if cfg.Feishu.OwnerOpenID != "ou_owner" {
		t.Fatalf("owner_open_id = %q", cfg.Feishu.OwnerOpenID)
	}
	if len(cfg.Feishu.AllowedSenders) != 1 || cfg.Feishu.AllowedSenders[0] != "ou_owner" {
		t.Fatalf("owner was not prefilled: %#v", cfg.Feishu.AllowedSenders)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Feishu.OwnerOpenID != "ou_owner" {
		t.Fatalf("saved owner_open_id = %q", loaded.Feishu.OwnerOpenID)
	}
	if !strings.Contains(out.String(), "pairing") {
		t.Fatalf("missing pairing hint: %s", out.String())
	}
}

func TestRunScriptedAllowlistRequiresSender(t *testing.T) {
	_, err := Run(t.Context(), Flags{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		AppID:      "cli_app",
		DMPolicy:   config.DMPolicyAllowlist,
	}, Options{
		Out: ioDiscard(),
		Env: func(string) string { return "secret" },
		Probe: func(context.Context, string, string, string) (feishu.Identity, error) {
			return feishu.Identity{OpenID: "ou_bot"}, nil
		},
		Owner: func(context.Context, string, string, string) (string, error) { return "", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "allowed_senders") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}

func TestRunInteractiveCollectsFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	in := strings.NewReader("2\ncli_prompt\n2\n2\nou_manual\n3\n")
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
	if cfg.Feishu.DMPolicy != config.DMPolicyAllowlist || cfg.Feishu.GroupPolicy != config.GroupPolicyDisabled {
		t.Fatalf("unexpected interactive config: %#v", cfg.Feishu)
	}
	if got := cfg.Feishu.AllowedSenders; len(got) != 1 || got[0] != "ou_manual" {
		t.Fatalf("senders = %#v", got)
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
	if len(cfg.Feishu.AllowedSenders) != 1 || cfg.Feishu.AllowedSenders[0] != "ou_scan" {
		t.Fatalf("scan open_id was not prefilled: %#v", cfg.Feishu.AllowedSenders)
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
	if cfg.Feishu.AppID != "cli_new" || cfg.Feishu.DMPolicy != config.DMPolicyPairing {
		t.Fatalf("unexpected config: %#v", cfg.Feishu)
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

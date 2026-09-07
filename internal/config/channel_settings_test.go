package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func channelPtr[T any](v T) *T { return &v }

func channelConfigFixture(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	c := &Config{
		Gateway:   Gateway{OwnerID: "console-owner", StatePath: filepath.Join(dir, "state.json"), DefaultChannel: "feishu"},
		Feishu:    Feishu{AppID: "app", AppSecret: "original-private-secret", OwnerOpenID: "im-owner", Domain: DomainFeishu, GroupPolicy: GroupPolicyOpen},
		Harnesses: map[string]Harness{"mock": {Command: "mock", Env: []string{"EXPLICIT=kept"}}},
		Agents:    map[string]Agent{"primary": {Harness: "mock", Default: true}},
		Projects:  map[string]Project{"work": {Home: ProjectHome{Path: dir}}},
	}
	path := filepath.Join(dir, "config.json")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, path
}

func TestExplicitChannelDisablePreservesStoredCredentials(t *testing.T) {
	disabled := false
	c := &Config{Gateway: Gateway{OwnerID: "console-owner", DefaultChannel: "console"}, Feishu: Feishu{Enabled: &disabled, AppID: "app", AppSecret: "secret"}}
	if c.FeishuEnabled() {
		t.Fatal("stored credentials override explicit channel disable")
	}
	if err := c.ValidateChannels(); err != nil {
		t.Fatal(err)
	}
	c.Feishu.AppSecret = ""
	if err := c.ValidateChannels(); err != nil {
		t.Fatalf("disabled channel cannot clear its secret while retaining AppID: %v", err)
	}
	c.Gateway.DefaultChannel = "feishu"
	if err := c.ValidateChannels(); err == nil {
		t.Fatal("disabled channel can remain the default")
	}
}

func TestChannelPatchPreservesIdentitySecretsAndExplicitClears(t *testing.T) {
	c, path := channelConfigFixture(t)
	c.Gateway.OwnerID = ""
	c.Feishu.AllowedSenders = []string{"allowed"}
	c.Feishu.BlockedSenders = []string{"blocked"}
	before, _ := json.Marshal(c)
	next, err := c.PatchChannels(ChannelPatch{DefaultChannel: channelPtr("console"), Feishu: &FeishuChannelPatch{Enabled: channelPtr(false), OwnerOpenID: channelPtr("new-im-owner"), AllowedSenders: channelPtr([]string{}), BlockedSenders: channelPtr([]string{})}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("patch mutated source configuration")
	}
	if next.Gateway.OwnerID != "im-owner" || next.Feishu.OwnerOpenID != "new-im-owner" || next.Feishu.AppSecret != c.Feishu.AppSecret || next.FeishuEnabled() || len(next.Feishu.AllowedSenders) != 0 || len(next.Feishu.BlockedSenders) != 0 {
		t.Fatal("patch changed console identity, lost secret or failed explicit clears")
	}
	if !reflect.DeepEqual(next.Harnesses, c.Harnesses) {
		t.Fatal("channel patch changed unrelated harness configuration")
	}
	if err := Save(path, next); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.FeishuEnabled() || reloaded.Feishu.AppSecret != c.Feishu.AppSecret || reloaded.EffectiveOwnerID() != "im-owner" {
		t.Fatal("disable or independent console identity did not survive reload")
	}
	cleared, err := reloaded.PatchChannels(ChannelPatch{Feishu: &FeishuChannelPatch{AppSecret: &ChannelSecret{Action: "clear"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cleared); err != nil {
		t.Fatal(err)
	}
	reloaded, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Feishu.AppSecret != "" || reloaded.ChannelSettings().Feishu.AppSecretConfigured {
		t.Fatal("explicit secret clear was not persisted")
	}
	reenabled, err := reloaded.PatchChannels(ChannelPatch{DefaultChannel: channelPtr("feishu"), Feishu: &FeishuChannelPatch{Enabled: channelPtr(true), Domain: channelPtr("lark"), AppSecret: &ChannelSecret{Action: "replace", Value: channelPtr("rotated-private-secret")}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reenabled.FeishuEnabled() || reenabled.Feishu.Domain != "lark" || reenabled.Feishu.AppSecret != "rotated-private-secret" {
		t.Fatal("explicit enable and secret replacement failed")
	}
	public, _ := json.Marshal(reenabled.ChannelSettings())
	if strings.Contains(string(public), "private-secret") || strings.Contains(string(public), `"app_secret":`) {
		t.Fatal("public channel settings contain secret value")
	}
}

func TestChannelPatchRejectsInvalidAndUnsafeCombinations(t *testing.T) {
	c, _ := channelConfigFixture(t)
	for _, tc := range []struct {
		name  string
		patch ChannelPatch
	}{
		{"disabled default", ChannelPatch{Feishu: &FeishuChannelPatch{Enabled: channelPtr(false)}}},
		{"clear enabled", ChannelPatch{Feishu: &FeishuChannelPatch{AppSecret: &ChannelSecret{Action: "clear"}}}},
		{"clear with value", ChannelPatch{Feishu: &FeishuChannelPatch{AppSecret: &ChannelSecret{Action: "clear", Value: channelPtr("do-not-echo")}}}},
		{"replace empty", ChannelPatch{Feishu: &FeishuChannelPatch{AppSecret: &ChannelSecret{Action: "replace", Value: channelPtr(" ")}}}},
		{"unknown secret action", ChannelPatch{Feishu: &FeishuChannelPatch{AppSecret: &ChannelSecret{Action: "do-not-echo"}}}},
		{"unknown default", ChannelPatch{DefaultChannel: channelPtr("do-not-echo")}},
		{"unknown domain", ChannelPatch{Feishu: &FeishuChannelPatch{Domain: channelPtr("do-not-echo")}}},
		{"unknown group policy", ChannelPatch{Feishu: &FeishuChannelPatch{GroupPolicy: channelPtr("do-not-echo")}}},
		{"empty allowed sender", ChannelPatch{Feishu: &FeishuChannelPatch{AllowedSenders: channelPtr([]string{" "})}}},
		{"empty blocked sender", ChannelPatch{Feishu: &FeishuChannelPatch{BlockedSenders: channelPtr([]string{""})}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.PatchChannels(tc.patch)
			if err == nil {
				t.Fatal("invalid update accepted")
			}
			if strings.Contains(err.Error(), "do-not-echo") {
				t.Fatal("validation echoed input that could contain a secret")
			}
		})
	}
	c.Gateway.OwnerID, c.Feishu.OwnerOpenID = "", ""
	if _, err := c.PatchChannels(ChannelPatch{}); err == nil {
		t.Fatal("channel edit without an existing console identity was accepted")
	}
}

func TestDisabledChannelLoadAndOptionValidation(t *testing.T) {
	c, path := channelConfigFixture(t)
	c.Feishu.Enabled = channelPtr(false)
	c.Gateway.DefaultChannel = ""
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Gateway.DefaultChannel != "console" || loaded.FeishuEnabled() {
		t.Fatal("stored credentials reactivated an explicitly disabled channel")
	}
	loaded.Feishu.Domain = "unsupported"
	if err := loaded.ValidateChannels(); err == nil {
		t.Fatal("disabled channel bypasses adapter option validation")
	}
	loaded.Feishu.Domain, loaded.Feishu.AppSecret = "feishu", ""
	loaded.Feishu.Enabled = channelPtr(true)
	if err := loaded.ValidateChannels(); err == nil {
		t.Fatal("enabled channel without credentials accepted")
	}
}

func TestChannelPatchPreservesLegacyGroupRestrictionUnlessExplicit(t *testing.T) {
	c, _ := channelConfigFixture(t)
	c.Feishu.AllowedSenders = []string{"allowed"}
	for _, patch := range []ChannelPatch{{}, {Feishu: &FeishuChannelPatch{Domain: channelPtr("lark")}}} {
		next, err := c.PatchChannels(patch)
		if err != nil || next.Feishu.GroupPolicy != GroupPolicyAllowlist {
			t.Fatalf("unrelated save widened old open+allowlist: %v", err)
		}
	}
	next, err := c.PatchChannels(ChannelPatch{Feishu: &FeishuChannelPatch{GroupPolicy: channelPtr(GroupPolicyOpen)}})
	if err != nil || next.Feishu.GroupPolicy != GroupPolicyOpen {
		t.Fatalf("explicit group policy was not honored: %v", err)
	}
	unchanged, err := next.PatchChannels(ChannelPatch{Feishu: &FeishuChannelPatch{Domain: channelPtr("lark")}})
	if err != nil || unchanged.Feishu.GroupPolicy != GroupPolicyOpen {
		t.Fatalf("unrelated edit undid explicit group policy: %v", err)
	}
}

func TestChannelDomainChangeDoesNotChangeConsoleLocale(t *testing.T) {
	c, path := channelConfigFixture(t)
	c.Feishu.Domain = DomainLark
	c.Gateway.Locale = ""
	next, err := c.PatchChannels(ChannelPatch{Feishu: &FeishuChannelPatch{Domain: channelPtr(DomainFeishu)}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Gateway.Locale != "en" || next.EffectiveLocale() != "en" || c.Gateway.Locale != "" {
		t.Fatal("IM domain change changed console locale or mutated source")
	}
	if err := Save(path, next); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil || reloaded.EffectiveLocale() != "en" {
		t.Fatal("independent locale did not survive reload", err)
	}
	reloaded.Gateway.Locale = "zh"
	next, err = reloaded.PatchChannels(ChannelPatch{Feishu: &FeishuChannelPatch{Domain: channelPtr(DomainLark)}})
	if err != nil || next.Gateway.Locale != "zh" {
		t.Fatal("explicit console locale was replaced", err)
	}
}

package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSettingsPolicyDefaultsValidationAndPatch(t *testing.T) {
	c := &Config{Gateway: Gateway{OwnerID: "owner"}}
	defaults := c.SettingsValues()
	if defaults.Policies.Execution.StepTimeout != Duration(15*time.Minute) || defaults.Policies.Snapshot.MaxBytes != 2*1024*1024*1024 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	for _, raw := range []string{`{"gateway":{"task_max_turns":-1}}`, `{"gateway":{"task_max_elapsed":"-1s"}}`, `{"gateway":{"prompt_timeout":"0s"}}`, `{"gateway":{"locale":"fr"}}`, `{"policies":{"execution":{"step_timeout":"0s"}}}`, `{"policies":{"planning":{"attempts":-1}}}`, `{"policies":{"snapshot":{"max_bytes":1,"max_file_bytes":2}}}`, `{"policies":{"review":{"max_changes":0}}}`, `{"feishu":{"app_secret":"x"}}`, `{} {}`} {
		if _, err := c.PatchSettings(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid settings accepted: %s", raw)
		}
	}
	next, err := c.PatchSettings(json.RawMessage(`{"gateway":{"task_max_turns":3,"locale":"en"},"policies":{"review":{"max_diff_bytes":4096}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.TaskMaxTurns != 0 || next.Gateway.TaskMaxTurns != 3 || next.Policies.Review.MaxDiffBytes != 4096 || next.Policies.Execution != defaults.Policies.Execution {
		t.Fatal("partial patch mutated source or omitted fields")
	}
}

func TestConsoleOnlyChannelValidationAndOwnerFallback(t *testing.T) {
	c := &Config{Gateway: Gateway{OwnerID: "owner", DefaultChannel: "console"}}
	if err := c.ValidateChannels(); err != nil {
		t.Fatal(err)
	}
	c.Feishu.AppID = "partial"
	if err := c.ValidateChannels(); err == nil {
		t.Fatal("partial credentials accepted")
	}
	c.Feishu.AppID = ""
	c.Gateway.OwnerID = ""
	if err := c.ValidateChannels(); err == nil {
		t.Fatal("anonymous console-only hub accepted")
	}
	c.Feishu.OwnerOpenID = "legacy-owner"
	if c.EffectiveOwnerID() != "legacy-owner" || c.EffectiveLocale() != "zh" {
		t.Fatal("legacy defaults lost")
	}
	c.Feishu.Domain = DomainLark
	if c.EffectiveLocale() != "en" {
		t.Fatal("legacy Lark locale fallback lost")
	}
	c.Gateway.Locale = "zh"
	if c.EffectiveLocale() != "zh" {
		t.Fatal("explicit locale tied to IM")
	}
}

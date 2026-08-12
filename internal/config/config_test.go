package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateFeishuCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agent":{"command":"mockagent"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Feishu.Validate()
	if err == nil || !strings.Contains(err.Error(), "feishu.app_id") {
		t.Fatalf("expected missing Feishu credentials error, got %v", err)
	}
}

func TestLoadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"agent": {"command": "mockagent"},
		"feishu": {"app_id": "app", "app_secret": "secret"},
		"gateway": {"prompt_timeout": "30s"}
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Gateway.PromptTimeout) != 30*time.Second {
		t.Fatalf("unexpected prompt timeout: %s", time.Duration(cfg.Gateway.PromptTimeout))
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"agent":{"command":"mockagent","commnad":"typo"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

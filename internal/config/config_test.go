package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateFeishuCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  command: mockagent\n"), 0o600); err != nil {
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

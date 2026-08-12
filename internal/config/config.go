package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentConfig describes how to launch and govern one ACP agent backend.
type AgentConfig struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	Workdir    string   `json:"workdir"`
	Env        []string `json:"env"`
	Permission string   `json:"permission"` // auto | always_allow | deny
}

type FeishuConfig struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

func (c FeishuConfig) Validate() error {
	if c.AppID == "" || c.AppSecret == "" {
		return fmt.Errorf("feishu.app_id and feishu.app_secret are required")
	}
	return nil
}

type GatewayConfig struct {
	PromptTimeout Duration `json:"prompt_timeout"`
}

type Config struct {
	Agent   AgentConfig   `json:"agent"`
	Feishu  FeishuConfig  `json:"feishu"`
	Gateway GatewayConfig `json:"gateway"`
}

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("parse duration: %w", err)
	}
	*d = Duration(parsed)
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse config: expected one JSON object")
	}
	if cfg.Agent.Command == "" {
		return nil, fmt.Errorf("agent.command is required")
	}
	if cfg.Agent.Permission == "" {
		cfg.Agent.Permission = "auto"
	}
	switch cfg.Agent.Permission {
	case "auto", "always_allow", "deny":
	default:
		return nil, fmt.Errorf("agent.permission must be auto, always_allow or deny")
	}
	if cfg.Gateway.PromptTimeout <= 0 {
		cfg.Gateway.PromptTimeout = Duration(10 * time.Minute)
	}
	if cfg.Agent.Workdir == "" {
		cfg.Agent.Workdir = "."
	}
	cfg.Agent.Workdir = expandHome(cfg.Agent.Workdir)
	abs, err := filepath.Abs(cfg.Agent.Workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	cfg.Agent.Workdir = abs
	return cfg, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

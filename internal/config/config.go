package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig describes how to launch and govern one ACP agent backend.
type AgentConfig struct {
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Workdir    string   `yaml:"workdir"`
	Env        []string `yaml:"env"`
	Permission string   `yaml:"permission"` // auto | always_allow | deny
}

type FeishuConfig struct {
	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`
}

func (c FeishuConfig) Validate() error {
	if c.AppID == "" || c.AppSecret == "" {
		return fmt.Errorf("feishu.app_id and feishu.app_secret are required")
	}
	return nil
}

type GatewayConfig struct {
	PromptTimeout time.Duration `yaml:"prompt_timeout"`
}

type Config struct {
	Agent   AgentConfig   `yaml:"agent"`
	Feishu  FeishuConfig  `yaml:"feishu"`
	Gateway GatewayConfig `yaml:"gateway"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
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
		cfg.Gateway.PromptTimeout = 10 * time.Minute
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

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

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
)

type Feishu struct {
	AppID          string   `json:"app_id"`
	AppSecret      string   `json:"app_secret"`
	AllowedSenders []string `json:"allowed_senders"`
}

func (c Feishu) Validate() error {
	if c.AppID == "" || c.AppSecret == "" {
		return fmt.Errorf("feishu.app_id and feishu.app_secret are required")
	}
	if len(c.AllowedSenders) == 0 {
		return fmt.Errorf("feishu.allowed_senders is required")
	}
	for _, sender := range c.AllowedSenders {
		if strings.TrimSpace(sender) == "" {
			return fmt.Errorf("feishu.allowed_senders cannot contain an empty ID")
		}
	}
	return nil
}

type Gateway struct {
	PromptTimeout Duration `json:"prompt_timeout"`
	StatePath     string   `json:"state_path"`
}

type Config struct {
	Agents     map[string]Agent     `json:"agents"`
	Harnesses  map[string]Harness   `json:"harnesses"`
	MCPServers map[string]MCPServer `json:"mcp_servers"`
	Feishu     Feishu               `json:"feishu"`
	Gateway    Gateway              `json:"gateway"`
}

type Agent struct {
	Aliases      []string `json:"aliases"`
	Harness      string   `json:"harness"`
	Workspace    string   `json:"workspace"`
	SystemPrompt string   `json:"system_prompt"`
	Skills       []string `json:"skills"`
	MCPServers   []string `json:"mcp_servers"`
	Default      bool     `json:"default"`
}

type Harness struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	ProcessDir string   `json:"process_dir"`
	Env        []string `json:"env"`
	Permission string   `json:"permission"`
}

type MCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
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

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func Starter(appID, appSecret, allowedSender string) *Config {
	return &Config{
		Agents: map[string]Agent{
			"codex": {
				Aliases: []string{"codex"}, Harness: "codex", Workspace: "~/steve-workspace/codex", Default: true,
			},
			"claude": {
				Aliases: []string{"claude"}, Harness: "claude-code", Workspace: "~/steve-workspace/claude",
			},
		},
		Harnesses: map[string]Harness{
			"codex": {
				Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}, Permission: "deny",
			},
			"claude-code": {
				Command: "npx", Args: []string{"-y", "@zed-industries/claude-code-acp"}, Permission: "deny",
			},
		},
		MCPServers: map[string]MCPServer{},
		Feishu: Feishu{
			AppID: appID, AppSecret: appSecret, AllowedSenders: []string{allowedSender},
		},
		Gateway: Gateway{
			PromptTimeout: Duration(10 * time.Minute), StatePath: "~/.steve/state.json",
		},
	}
}

func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	complete = true
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
	if cfg.Gateway.PromptTimeout <= 0 {
		cfg.Gateway.PromptTimeout = Duration(10 * time.Minute)
	}
	if cfg.Gateway.StatePath == "" {
		cfg.Gateway.StatePath = "~/.steve/state.json"
	}
	cfg.Gateway.StatePath = absolute(cfg.Gateway.StatePath)
	for id, item := range cfg.Agents {
		if item.Workspace == "" {
			return nil, fmt.Errorf("agent %q workspace is required", id)
		}
		item.Workspace = absolute(item.Workspace)
		for i, skill := range item.Skills {
			item.Skills[i] = absolute(skill)
		}
		cfg.Agents[id] = item
	}
	for id, item := range cfg.Harnesses {
		if item.Permission == "" {
			item.Permission = "deny"
		}
		item.ProcessDir = absolute(item.ProcessDir)
		cfg.Harnesses[id] = item
	}
	if _, err := cfg.AgentCatalog(); err != nil {
		return nil, err
	}
	if _, err := cfg.HarnessManager(); err != nil {
		return nil, err
	}
	for id, item := range cfg.Agents {
		if _, ok := cfg.Harnesses[item.Harness]; !ok {
			return nil, fmt.Errorf("agent %q references unknown harness %q", id, item.Harness)
		}
		for _, server := range item.MCPServers {
			if _, ok := cfg.MCPServers[server]; !ok {
				return nil, fmt.Errorf("agent %q references unknown MCP server %q", id, server)
			}
		}
	}
	return cfg, nil
}

func (c *Config) AgentCatalog() (*agent.Catalog, error) {
	configs := make(map[string]agent.Config, len(c.Agents))
	for id, item := range c.Agents {
		configs[id] = agent.Config{
			Harness: item.Harness, Aliases: item.Aliases, Workspace: item.Workspace,
			SystemPrompt: item.SystemPrompt, Skills: item.Skills, MCPServers: item.MCPServers, Default: item.Default,
		}
	}
	return agent.NewCatalog(configs)
}

func (c *Config) HarnessManager() (*harness.Manager, error) {
	configs := make(map[string]harness.Config, len(c.Harnesses))
	for id, item := range c.Harnesses {
		configs[id] = harness.Config{
			Command: item.Command, Args: item.Args, ProcessDir: item.ProcessDir, Env: item.Env, Permission: item.Permission,
		}
	}
	return harness.NewManager(configs)
}

func (c *Config) CapabilityAssembler() *capability.Assembler {
	servers := make(map[string]capability.MCPServer, len(c.MCPServers))
	for id, item := range c.MCPServers {
		servers[id] = capability.MCPServer{
			Type: item.Type, Command: item.Command, Args: item.Args, Env: item.Env, URL: item.URL, Headers: item.Headers,
		}
	}
	return capability.NewAssembler(servers)
}

func absolute(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimPrefix(path, "~")
			path = filepath.Join(home, strings.TrimPrefix(rest, "/"))
		}
	}
	result, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return result
}

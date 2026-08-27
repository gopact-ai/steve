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

const (
	DomainFeishu = "feishu"
	DomainLark   = "lark"

	// Legacy dm_policy values still accepted in JSON and ignored.
	DMPolicyPairing   = "pairing"
	DMPolicyAllowlist = "allowlist"

	GroupPolicyAllowlist = "allowlist"
	GroupPolicyOpen      = "open"
	GroupPolicyDisabled  = "disabled"

	PermissionRead        = "read"
	PermissionWrite       = "write"
	PermissionDeny        = "deny"
	PermissionAuto        = "auto"
	PermissionAlwaysAllow = "always_allow"
)

type Feishu struct {
	AppID            string   `json:"app_id"`
	AppSecret        string   `json:"app_secret"`
	Domain           string   `json:"domain,omitempty"`
	DMPolicy         string   `json:"dm_policy,omitempty"`
	AllowedSenders   []string `json:"allowed_senders,omitempty"`
	BlockedSenders   []string `json:"blocked_senders,omitempty"`
	GroupPolicy      string   `json:"group_policy,omitempty"`
	AllowUnmentioned bool     `json:"allow_unmentioned,omitempty"`
	OwnerOpenID      string   `json:"owner_open_id,omitempty"`
}

func (c *Feishu) applyDefaults() {
	if c.Domain == "" {
		c.Domain = DomainFeishu
	}
	if c.GroupPolicy == "" {
		c.GroupPolicy = GroupPolicyOpen
	}
}

func (c Feishu) Validate() error {
	c.applyDefaults()
	if c.AppID == "" || c.AppSecret == "" {
		return fmt.Errorf("feishu.app_id and feishu.app_secret are required")
	}
	switch c.Domain {
	case DomainFeishu, DomainLark:
	default:
		return fmt.Errorf("feishu.domain must be %q or %q", DomainFeishu, DomainLark)
	}
	if c.DMPolicy != "" && c.DMPolicy != DMPolicyPairing && c.DMPolicy != DMPolicyAllowlist {
		return fmt.Errorf("feishu.dm_policy must be %q or %q", DMPolicyPairing, DMPolicyAllowlist)
	}
	switch c.GroupPolicy {
	case GroupPolicyAllowlist, GroupPolicyOpen, GroupPolicyDisabled:
	default:
		return fmt.Errorf("feishu.group_policy must be %q, %q, or %q", GroupPolicyAllowlist, GroupPolicyOpen, GroupPolicyDisabled)
	}
	if err := validateIDs("feishu.allowed_senders", c.AllowedSenders); err != nil {
		return err
	}
	return validateIDs("feishu.blocked_senders", c.BlockedSenders)
}

func validateIDs(field string, ids []string) error {
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%s cannot contain an empty id", field)
		}
	}
	return nil
}

type Gateway struct {
	PromptTimeout Duration `json:"prompt_timeout"`
	StatePath     string   `json:"state_path"`
	HomePath      string   `json:"home_path,omitempty"`
	// TaskMaxTurns and TaskMaxElapsed raise the per-task budget for long
	// running work; zero keeps the built-in defaults.
	TaskMaxTurns   int      `json:"task_max_turns,omitempty"`
	TaskMaxElapsed Duration `json:"task_max_elapsed,omitempty"`
	// DebugAddr enables a loopback-only endpoint for injecting messages and
	// card callbacks; empty keeps it off. DebugChatID is the chat those
	// injected messages default to.
	DebugAddr   string `json:"debug_addr,omitempty"`
	DebugChatID string `json:"debug_chat_id,omitempty"`
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
	feishu := Feishu{AppID: appID, AppSecret: appSecret}
	if allowedSender != "" {
		feishu.AllowedSenders = []string{allowedSender}
	}
	return StarterFeishu(feishu)
}

func StarterFeishu(feishu Feishu) *Config {
	feishu.applyDefaults()
	return &Config{
		Agents: map[string]Agent{
			harness.Codex: {
				Aliases: []string{harness.Codex}, Harness: harness.Codex, Workspace: "~/steve-workspace/codex", Default: true,
			},
			"claude": {
				Aliases: []string{"claude"}, Harness: harness.ClaudeCode, Workspace: "~/steve-workspace/claude",
			},
			harness.Grok: {
				Aliases: []string{harness.Grok, "grok-build"}, Harness: harness.Grok, Workspace: "~/steve-workspace/grok",
			},
			harness.Kimi: {
				Aliases: []string{harness.Kimi, "kimi-code"}, Harness: harness.Kimi, Workspace: "~/steve-workspace/kimi",
			},
		},
		Harnesses: map[string]Harness{
			harness.Codex: {
				Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}, Permission: PermissionRead,
			},
			harness.ClaudeCode: {
				Command: "npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}, Permission: PermissionRead,
			},
			harness.Grok: {
				Command: "grok", Args: []string{"agent", "--no-leader", "stdio"}, Permission: PermissionRead,
			},
			harness.Kimi: {
				Command: "kimi", Args: []string{"acp"}, Permission: PermissionRead,
			},
		},
		MCPServers: map[string]MCPServer{},
		Feishu:     feishu,
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
	temp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	name := temp.Name()
	if err := writeConfigFile(temp, append(data, '\n')); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace config: %w", err)
	}
	return syncDir(dir)
}

// syncDir flushes a directory entry after a rename so the replacement
// survives a crash (rename alone is not guaranteed durable on all filesystems).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeConfigFile(file *os.File, data []byte) error {
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("chmod config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
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
	cfg.Feishu.applyDefaults()
	cfg.Feishu.OwnerOpenID = strings.TrimSpace(cfg.Feishu.OwnerOpenID)
	if cfg.Gateway.PromptTimeout <= 0 {
		cfg.Gateway.PromptTimeout = Duration(10 * time.Minute)
	}
	if cfg.Gateway.StatePath == "" {
		cfg.Gateway.StatePath = "~/.steve/state.json"
	}
	cfg.Gateway.StatePath = absolute(cfg.Gateway.StatePath)
	if cfg.Gateway.HomePath == "" {
		cfg.Gateway.HomePath = filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "home")
	}
	cfg.Gateway.HomePath = absolute(cfg.Gateway.HomePath)
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
			item.Permission = PermissionRead
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

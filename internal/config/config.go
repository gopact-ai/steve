package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
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

// DefaultOfflineReminder is the threshold a turn has to cross before its
// answer is also announced in plain text. Fifteen minutes is well past the
// point where someone keeps watching a chat window.
const DefaultOfflineReminder = 15 * time.Minute

type Gateway struct {
	// DefaultChannel fills only a missing channel on an authorized message anchor.
	DefaultChannel string `json:"default_channel,omitempty"`
	// NodeBinary is a steve-node executable the hub can hand to a machine
	// being added (static build, the nodes' architecture); empty means the
	// bootstrap script expects the binary to be there already.
	NodeBinary string `json:"node_binary,omitempty"`
	// PromptTimeout is how long a turn may go silent — no tool call, no
	// text, no report — before it is cut. It is not a cap on the turn: a
	// turn that awaits other agents runs as long as they keep answering.
	PromptTimeout Duration `json:"prompt_timeout"`
	StatePath     string   `json:"state_path"`
	HomePath      string   `json:"home_path,omitempty"`
	// TaskMaxTurns and TaskMaxElapsed raise the per-task budget for long
	// running work; zero keeps the built-in defaults.
	TaskMaxTurns   int      `json:"task_max_turns,omitempty"`
	TaskMaxElapsed Duration `json:"task_max_elapsed,omitempty"`
	// OfflineReminderAfter is how long a turn must run before its answer
	// also earns a plain-text ping at the anchor: past that, the asker has
	// probably walked away, and a card arriving quietly is a delivery that
	// did not happen. Negative turns it off; zero takes the default.
	OfflineReminderAfter Duration `json:"offline_reminder_after,omitempty"`
	// DebugAddr enables a loopback-only endpoint for injecting messages and
	// card callbacks; empty keeps it off. DebugChatID is the chat those
	// injected messages default to.
	DebugAddr   string `json:"debug_addr,omitempty"`
	DebugChatID string `json:"debug_chat_id,omitempty"`
	// Capabilities are what the hub's own machine offers. Hub-local agents
	// are not exempt from capability matching: running work here because
	// here is the default is how a GPU step ends up on a box without one.
	Capabilities []string `json:"capabilities,omitempty"`
	// Tools are binaries the hub machine should look for; Declares are
	// capabilities taken on the operator's word ("network:internal").
	Tools    []string `json:"tools,omitempty"`
	Declares []string `json:"declares,omitempty"`
	// ReadModelAddr serves the snapshot, the change stream and the
	// dashboard. It defaults to loopback; anywhere else needs a token,
	// because the snapshot names hosts, goals and agents.
	ReadModelAddr  string `json:"read_model_addr,omitempty"`
	ReadModelToken string `json:"read_model_token,omitempty"`
	// Planner names the agent that decomposes /plan goals. Empty keeps the
	// rule planner, which places but does not decompose: an open goal is
	// then one step, which is honest but not automatic.
	Planner string `json:"planner,omitempty"`
	// Level is the hub machine's own data level. Empty is internal.
	Level string `json:"level,omitempty"`
	// Region names this hub's region; Regions lists the other hubs whose
	// leases this one must honour; IssuerAddr and IssuerToken serve this
	// hub's own leases to them.
	Region      string            `json:"region,omitempty"`
	Regions     map[string]Region `json:"regions,omitempty"`
	IssuerAddr  string            `json:"issuer_addr,omitempty"`
	IssuerToken string            `json:"issuer_token,omitempty"`
	// DirectTransfer lets artifacts move node to node when a node already
	// holds them, the hub granting one transfer at a time; off means every
	// byte goes through the hub.
	DirectTransfer bool `json:"direct_transfer,omitempty"`
	// DefaultProject is what a conversation is bound to on its first turn
	// when nobody has said otherwise. With one project it is implied.
	DefaultProject string `json:"default_project,omitempty"`
}

// ReservedHomeProject is the project the gateway declares for its own home
// directory; a config may not claim the name.
const ReservedHomeProject = "home"

// Project is the operator's declaration of a project: where its canonical
// workspace lives and the facts the hub assigns to it.
type Project struct {
	Home           ProjectHome `json:"home"`
	Level          string      `json:"level,omitempty"`
	Repo           string      `json:"repo,omitempty"`
	Skills         []string    `json:"skills,omitempty"`
	DurablePlaces  []string    `json:"durable_places,omitempty"`
	ExternalRemote string      `json:"external_remote,omitempty"`
	// Grants maps a principal (Feishu open_id) to a role: read, write or
	// admin. DefaultRole is what everyone else gets.
	Grants      map[string]string `json:"grants,omitempty"`
	DefaultRole string            `json:"default_role,omitempty"`
	// Workspaces are the project's copies: directories on machines other
	// than its home where interactive turns may run. Each is adopted as
	// it is; cloning happens before it is written here.
	Workspaces []ProjectWorkspace `json:"workspaces,omitempty"`
}

// ProjectWorkspace is one copy of a project: a machine and a directory.
type ProjectWorkspace struct {
	Node string `json:"node,omitempty"`
	Path string `json:"path"`
}

// ProjectHome is the (node, path) of a project's canonical workspace. An
// empty node is the hub; a path on another node is that node's path and
// is never resolved against the hub's filesystem.
type ProjectHome struct {
	Node string `json:"node,omitempty"`
	Path string `json:"path"`
}

type Config struct {
	Agents     map[string]Agent     `json:"agents"`
	Projects   map[string]Project   `json:"projects,omitempty"`
	Harnesses  map[string]Harness   `json:"harnesses"`
	Nodes      map[string]Node      `json:"nodes,omitempty"`
	MCPServers map[string]MCPServer `json:"mcp_servers"`
	Feishu     Feishu               `json:"feishu"`
	Gateway    Gateway              `json:"gateway"`
	// Migrated lists what Load rewrote from an older layout, for the
	// operator to move into the file: the runtime never reads the old
	// fields again.
	Migrated []string `json:"-"`
}

// Node is one remote machine running steve-node. Everything about what it
// can run comes from its advert on connect, never from here: this is only
// how to reach it and how to prove we may.
type Node struct {
	Addr  string   `json:"addr"`
	Token string   `json:"token"`
	Dial  Duration `json:"dial_timeout,omitempty"`
	// Level is the data level the hub assigns the node: the highest a
	// project may be for its artifacts to be held or run there. Empty is
	// internal.
	Level string `json:"level,omitempty"`
	// Region is whose leases the node's resources carry. Empty is this
	// hub's own region; another region must be listed in gateway.regions.
	Region string `json:"region,omitempty"`
	// PeerAddr is where other nodes reach this one for direct transfers;
	// empty means addr.
	PeerAddr string `json:"peer_addr,omitempty"`
}

// Region is another hub that issues leases for its own machines.
type Region struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type Agent struct {
	Aliases []string `json:"aliases"`
	Harness string   `json:"harness"`
	// Node places this agent on a machine; empty runs it on the hub.
	Node string `json:"node,omitempty"`
	// Model is the preferred model. The node's advert decides what is
	// really on offer there. Options pins other selectors by option id
	// (reasoning effort, thinking, mode); About says what the agent is for.
	Model   string            `json:"model,omitempty"`
	Options map[string]string `json:"options,omitempty"`
	About   string            `json:"about,omitempty"`
	// Requires are capabilities the node must advertise.
	Requires []string `json:"requires,omitempty"`
	// LegacyWorkspace is the pre-project per-agent directory. It is read
	// only to migrate into projects{}; an agent has no directory of its
	// own — a conversation's project decides where work happens.
	LegacyWorkspace string   `json:"workspace,omitempty"`
	SystemPrompt    string   `json:"system_prompt"`
	Skills          []string `json:"skills"`
	MCPServers      []string `json:"mcp_servers"`
	Default         bool     `json:"default"`
}

type Harness struct {
	// Adapter names an ACP adapter from the built-in catalog, which Steve
	// fetches at a pinned version and verifies before running. Give this
	// or Command, not both: an explicit command is a machine's own build,
	// and Steve does not second-guess it.
	Adapter    string   `json:"adapter,omitempty"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	ProcessDir string   `json:"process_dir"`
	Env        []string `json:"env"`
	Permission string   `json:"permission"`
	// Slots caps concurrent sessions of this harness on the hub; zero is
	// unlimited. Nodes declare their own in their config.
	Slots int `json:"slots,omitempty"`
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
				Aliases: []string{harness.Codex}, Harness: harness.Codex, Default: true,
			},
			"claude": {
				Aliases: []string{"claude"}, Harness: harness.ClaudeCode,
			},
			harness.Grok: {
				Aliases: []string{harness.Grok, "grok-build"}, Harness: harness.Grok,
			},
			harness.Kimi: {
				Aliases: []string{harness.Kimi, "kimi-code"}, Harness: harness.Kimi,
			},
		},
		Projects: map[string]Project{
			"workspace": {Home: ProjectHome{Path: "~/steve-workspace"}},
		},
		Harnesses: map[string]Harness{
			harness.Codex: {
				Adapter: "codex-acp", Permission: PermissionRead,
			},
			harness.ClaudeCode: {
				Adapter: "claude-agent-acp", Permission: PermissionRead,
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
	if cfg.Gateway.DefaultChannel == "" {
		cfg.Gateway.DefaultChannel = "feishu"
	}
	cfg.Feishu.applyDefaults()
	cfg.Feishu.OwnerOpenID = strings.TrimSpace(cfg.Feishu.OwnerOpenID)
	if cfg.Gateway.PromptTimeout <= 0 {
		cfg.Gateway.PromptTimeout = Duration(10 * time.Minute)
	}
	if cfg.Gateway.OfflineReminderAfter == 0 {
		cfg.Gateway.OfflineReminderAfter = Duration(DefaultOfflineReminder)
	}
	if cfg.Gateway.StatePath == "" {
		cfg.Gateway.StatePath = "~/.steve/state.json"
	}
	if cfg.Gateway.ReadModelAddr == "" {
		cfg.Gateway.ReadModelAddr = "127.0.0.1:7710"
	}
	cfg.Gateway.StatePath = absolute(cfg.Gateway.StatePath)
	if cfg.Gateway.HomePath == "" {
		cfg.Gateway.HomePath = filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "home")
	}
	cfg.Gateway.HomePath = absolute(cfg.Gateway.HomePath)
	for id, item := range cfg.Agents {
		for i, skill := range item.Skills {
			item.Skills[i] = absolute(skill)
		}
		cfg.Agents[id] = item
	}
	if err := cfg.migrateProjects(); err != nil {
		return nil, err
	}
	for id, item := range cfg.Projects {
		if id == ReservedHomeProject {
			return nil, fmt.Errorf("project id %q is reserved for Steve's own home directory", id)
		}
		if item.Home.Path == "" {
			return nil, fmt.Errorf("project %q home.path is required", id)
		}
		// A remote home is a path on the node's filesystem. Resolving it
		// against the hub's home would produce a path that means something
		// different — or nothing — over there.
		if item.Home.Node == "" {
			item.Home.Path = absolute(item.Home.Path)
		}
		if item.Home.Node != "" {
			if _, ok := cfg.Nodes[item.Home.Node]; !ok {
				return nil, fmt.Errorf("project %q home.node %q is not in nodes{}", id, item.Home.Node)
			}
		}
		for i, skill := range item.Skills {
			item.Skills[i] = absolute(skill)
		}
		cfg.Projects[id] = item
	}
	for id, item := range cfg.Nodes {
		if item.Region != "" && item.Region != cfg.Gateway.Region && cfg.Gateway.Region != "" || item.Region != "" && cfg.Gateway.Region == "" && item.Region != "default" {
			if _, ok := cfg.Gateway.Regions[item.Region]; !ok {
				return nil, fmt.Errorf("node %q is in region %q, which gateway.regions does not list", id, item.Region)
			}
		}
		if item.Level != "" && !project.Level(item.Level).Valid() {
			return nil, fmt.Errorf("node %q level %q is not public, internal, restricted or sealed", id, item.Level)
		}
	}
	if cfg.Gateway.Level != "" && !project.Level(cfg.Gateway.Level).Valid() {
		return nil, fmt.Errorf("gateway.level %q is not public, internal, restricted or sealed", cfg.Gateway.Level)
	}
	if cfg.Gateway.DefaultProject == "" && len(cfg.Projects) == 1 {
		for id := range cfg.Projects {
			cfg.Gateway.DefaultProject = id
		}
	}
	if cfg.Gateway.DefaultProject != "" {
		if _, ok := cfg.Projects[cfg.Gateway.DefaultProject]; !ok {
			return nil, fmt.Errorf("gateway.default_project %q is not in projects{}", cfg.Gateway.DefaultProject)
		}
	}
	for id, item := range cfg.Harnesses {
		if item.Permission == "" {
			item.Permission = PermissionRead
		}
		switch {
		case item.Adapter != "" && item.Command != "":
			return nil, fmt.Errorf("harness %q sets both adapter and command; pick one", id)
		case item.Adapter == "" && item.Command == "":
			return nil, fmt.Errorf("harness %q needs an adapter or a command", id)
		case item.Adapter != "":
			if _, known := adapter.Catalog[item.Adapter]; !known {
				return nil, fmt.Errorf("harness %q: adapter %q is not one of %s", id, item.Adapter, strings.Join(adapter.Names(), ", "))
			}
			if len(item.Args) > 0 {
				return nil, fmt.Errorf("harness %q: an adapter takes no args", id)
			}
		}
		item.ProcessDir = absolute(item.ProcessDir)
		cfg.Harnesses[id] = item
	}
	if cfg.Gateway.Planner != "" {
		if _, ok := cfg.Agents[cfg.Gateway.Planner]; !ok {
			return nil, fmt.Errorf("gateway.planner references unknown agent %q", cfg.Gateway.Planner)
		}
	}
	for id, item := range cfg.Nodes {
		if strings.TrimSpace(item.Addr) == "" {
			return nil, fmt.Errorf("node %q addr is required", id)
		}
		if strings.TrimSpace(item.Token) == "" {
			return nil, fmt.Errorf("node %q token is required", id)
		}
	}
	if _, err := cfg.AgentCatalog(); err != nil {
		return nil, err
	}
	// Not HarnessManager: a harness that names an adapter has no command
	// until the adapter is fetched, and loading a file must not depend on
	// the network. Loading checks the configuration; the manager checks
	// that it can be run.
	if err := cfg.validateHarnesses(); err != nil {
		return nil, err
	}
	for id, item := range cfg.Agents {
		if _, ok := cfg.Harnesses[item.Harness]; !ok {
			return nil, fmt.Errorf("agent %q references unknown harness %q", id, item.Harness)
		}
		if item.Node != "" {
			if _, ok := cfg.Nodes[item.Node]; !ok {
				return nil, fmt.Errorf("agent %q references unknown node %q", id, item.Node)
			}
		}
		for _, server := range item.MCPServers {
			// An agent on another machine uses that machine's servers,
			// bound there at admission; only a hub-local agent's names must
			// exist in this configuration.
			if _, ok := cfg.MCPServers[server]; !ok && item.Node == "" {
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
			Harness: item.Harness, Node: item.Node, Model: item.Model, Options: item.Options, About: item.About, Requires: item.Requires,
			Aliases:      item.Aliases,
			SystemPrompt: item.SystemPrompt, Skills: item.Skills, MCPServers: item.MCPServers, Default: item.Default,
		}
	}
	return agent.NewCatalog(configs)
}

func (c *Config) validateHarnesses() error {
	if len(c.Harnesses) == 0 {
		return fmt.Errorf("at least one harness is required")
	}
	for id, item := range c.Harnesses {
		if id == "" {
			return fmt.Errorf("a harness needs a name")
		}
		if _, err := permission.New(item.Permission); err != nil {
			return fmt.Errorf("harness %q: %w", id, err)
		}
	}
	return nil
}

// PrepareAdapters fetches and verifies every adapter the configuration
// names, filling in the command that starts it. It runs before the harness
// manager is built, so a machine either has the pinned adapter or refuses
// to start with a reason — there is no version to discover later.
func (c *Config) PrepareAdapters(ctx context.Context) error {
	install := &adapter.Installer{Dir: c.AdapterDir()}
	for id, item := range c.Harnesses {
		if item.Adapter == "" {
			continue
		}
		got, err := install.Ensure(ctx, item.Adapter)
		if err != nil {
			return fmt.Errorf("harness %q: %w", id, err)
		}
		if !got.Cached {
			log.Printf("adapter: installed %s@%s for harness %s", got.Package, got.Version, id)
		}
		item.Command = got.Command
		c.Harnesses[id] = item
	}
	return nil
}

// AdapterDir is where fetched adapters live: beside the ledger, because
// they are part of this deployment's state, not of anyone's home.
func (c *Config) AdapterDir() string {
	return filepath.Join(filepath.Dir(absolute(c.Gateway.StatePath)), "adapters")
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

// NodeConfigs is what the registry needs to reach each remote machine.
func (c *Config) NodeConfigs() map[string]node.Config {
	out := make(map[string]node.Config, len(c.Nodes))
	for id, item := range c.Nodes {
		out[id] = node.Config{Addr: item.Addr, Token: item.Token, DialTimeout: time.Duration(item.Dial), Level: item.Level, Region: item.Region, PeerAddr: item.PeerAddr}
	}
	return out
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

// migrateProjects turns the pre-project layout — a workspace on every
// agent — into projects{}: one project per agent, homed where the agent
// ran, the default agent's as the default project. It runs only when the
// file has no projects{} of its own; a file with both is refused so the two
// cannot disagree.
func (c *Config) migrateProjects() error {
	legacy := map[string]Agent{}
	for id, item := range c.Agents {
		if item.LegacyWorkspace != "" {
			legacy[id] = item
		}
	}
	if len(legacy) == 0 {
		if len(c.Projects) == 0 {
			return fmt.Errorf("projects{} is required: at least one project with a home path (agents[].workspace moved there)")
		}
		return nil
	}
	if len(c.Projects) > 0 {
		return fmt.Errorf("agents[].workspace is no longer read; remove it, the project's home path in projects{} is what counts")
	}
	c.Projects = map[string]Project{}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := legacy[id]
		c.Projects[id] = Project{Home: ProjectHome{Node: item.Node, Path: item.LegacyWorkspace}}
		if item.Default && c.Gateway.DefaultProject == "" {
			c.Gateway.DefaultProject = id
		}
		item.LegacyWorkspace = ""
		c.Agents[id] = item
	}
	rendered, _ := json.MarshalIndent(c.Projects, "", "  ")
	c.Migrated = append(c.Migrated, fmt.Sprintf("agents[].workspace is now projects{}; move this into the config and set gateway.default_project = %q:\n%s", c.Gateway.DefaultProject, rendered))
	return nil
}

// ProjectList renders projects{} as the runtime's records.
func (c *Config) ProjectList() []project.Project {
	out := make([]project.Project, 0, len(c.Projects))
	for id, item := range c.Projects {
		p := project.Project{
			ID: id, Level: project.Level(item.Level), Repo: project.RepoMode(item.Repo), Skills: item.Skills,
			DurablePlaces: item.DurablePlaces, ExternalRemote: item.ExternalRemote, DefaultRole: project.Role(item.DefaultRole),
			Home: project.Home{Node: item.Home.Node, Path: item.Home.Path},
		}
		for _, ws := range item.Workspaces {
			if p.Copies == nil {
				p.Copies = map[string]project.Copy{}
			}
			p.Copies[ws.Node] = project.Copy{Node: ws.Node, Path: ws.Path}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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

// HubSlots is the per-harness session cap on the hub.
func (c *Config) HubSlots() map[string]int {
	out := map[string]int{}
	for id, h := range c.Harnesses {
		if h.Slots > 0 {
			out[id] = h.Slots
		}
	}
	return out
}

// NodeLevels is the level the hub assigned each node.
func (c *Config) NodeLevels() map[string]project.Level {
	out := map[string]project.Level{}
	for id, n := range c.Nodes {
		out[id] = project.Level(n.Level).OrDefault()
	}
	return out
}

// HubLevel is the hub machine's data level: what was configured, or else
// the highest level of anything the hub is the durable place for — every
// project except a sealed one homed on a node, and Steve's own home, which
// is restricted. A hub that could not hold what it stores would refuse
// its own projects at the first turn.
func (c *Config) HubLevel() project.Level {
	if c.Gateway.Level != "" {
		return project.Level(c.Gateway.Level)
	}
	level := project.LevelRestricted
	for _, p := range c.Projects {
		candidate := project.Level(p.Level).OrDefault()
		if candidate == project.LevelSealed && p.Home.Node != "" {
			continue
		}
		if !candidate.Admits(level) {
			level = candidate
		}
	}
	return level
}

// GrantList renders every configured grant.
func (c *Config) GrantList() []project.Grant {
	var out []project.Grant
	for id, item := range c.Projects {
		for principal, role := range item.Grants {
			out = append(out, project.Grant{Project: id, Principal: principal, Role: project.Role(role), By: "config"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Principal < out[j].Principal
	})
	return out
}

// NodeRegions is the region of each node; unset means the hub's own.
func (c *Config) NodeRegions() map[string]string {
	out := map[string]string{}
	for id, n := range c.Nodes {
		out[id] = n.Region
	}
	return out
}

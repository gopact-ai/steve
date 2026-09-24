package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/fsx"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
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
	// Enabled separates connection state from stored credentials. Omitted
	// retains credential-based activation for existing configuration files.
	Enabled          *bool    `json:"enabled,omitempty"`
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
	if c.AppID == "" || c.AppSecret == "" {
		return fmt.Errorf("feishu.app_id and feishu.app_secret are required")
	}
	return c.validateOptions()
}

func (c Feishu) validateOptions() error {
	c.applyDefaults()
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
	HubID string             `json:"hub_id,omitempty"`
	Peers map[string]HubPeer `json:"peers,omitempty"`
	// Locale and OwnerID are console-level defaults, independent of an IM.
	Locale  string `json:"locale,omitempty"`
	OwnerID string `json:"owner_id,omitempty"`
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
	// RecoveryQuiet is how long a recovery rejoins a node it lost on its
	// own before the owner is asked what to do with the work. A node
	// restart or a dropped link lasts seconds and the execution keeps
	// running through it, so a card about it would be noise.
	RecoveryQuiet Duration `json:"recovery_quiet,omitempty"`
	StatePath     string   `json:"state_path"`
	HomePath      string   `json:"home_path,omitempty"`
	// WorkspaceRoot is the directory this machine keeps its work in: the
	// one its owner chose, with every project under it. It describes this
	// machine's filesystem and is never taken from a shared declaration,
	// which names directories on whichever machine wrote it. Empty falls
	// back to the state directory, which is where a fresh installation
	// works until its owner picks somewhere.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// DefaultApproval is the fleet-wide approval stance: "ask", "auto",
	// "full", or empty for none. Every AI tool names its approval levels
	// itself, so this is an intent resolved against whatever modes an
	// agent turns out to offer, and an agent that pins its own mode keeps
	// it. See internal/approval.
	DefaultApproval string `json:"default_approval,omitempty"`
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

type HubPeer struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url"`
	Token string `json:"token"`
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
	// Grants maps a principal to a configured role, including explicit none.
	// Configured roles override runtime grants. Removing a configured entry
	// reveals the retained runtime grant, then DefaultRole/the level default.
	Grants      map[string]string `json:"grants,omitempty"`
	DefaultRole string            `json:"default_role,omitempty"`
	// Workspaces declare the project's copies. Clone origin/source describe
	// intent; provisioning progress remains in the project ledger projection.
	Workspaces []ProjectWorkspace `json:"workspaces,omitempty"`
}

// ProjectWorkspace is one copy of a project: a machine and a directory.
type ProjectWorkspace struct {
	Node   string `json:"node,omitempty"`
	Path   string `json:"path"`
	Origin string `json:"origin,omitempty"`
	Source string `json:"source,omitempty"`
}

// ProjectHome is the (node, path) of a project's canonical workspace. An
// empty node is the hub; a path on another node is that node's path and
// is never resolved against the hub's filesystem.
type ProjectHome struct {
	Node string `json:"node,omitempty"`
	Path string `json:"path"`
}

type Config struct {
	Plugins map[string]plugins.Installation `json:"plugins,omitempty"`
	// RuntimePermissions are shared execution policies, without local commands.
	RuntimePermissions map[string]string `json:"-"`
	// RuntimeHome keeps the shared home workspace anchored to its physical
	// node while this process uses its own local identity files.
	RuntimeHome       *ProjectHome         `json:"-"`
	Policies          Policies             `json:"policies,omitempty"`
	Agents            map[string]Agent     `json:"agents"`
	Projects          map[string]Project   `json:"projects,omitempty"`
	Harnesses         map[string]Harness   `json:"harnesses"`
	Nodes             map[string]Node      `json:"nodes,omitempty"`
	MCPServers        map[string]MCPServer `json:"mcp_servers"`
	Feishu            Feishu               `json:"feishu"`
	Gateway           Gateway              `json:"gateway"`
	sourcePath        string
	sourceFingerprint string
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
	PluginOrigin *plugins.AgentOrigin `json:"plugin_origin,omitempty"`
	Aliases      []string             `json:"aliases"`
	Harness      string               `json:"harness"`
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
	SystemPrompt string   `json:"system_prompt"`
	Skills       []string `json:"skills"`
	MCPServers   []string `json:"mcp_servers"`
	Default      bool     `json:"default"`
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

// DefaultRecoveryQuiet covers a node restart or a dropped link without
// asking anyone, and still reports a node that is really gone while its
// owner remembers asking for the work.
const DefaultRecoveryQuiet = 90 * time.Second

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
			"codex": {
				Aliases: []string{"codex"}, Harness: "codex", Default: true,
			},
			"claude": {
				Aliases: []string{"claude"}, Harness: "claude-code",
			},
			"grok": {
				Aliases: []string{"grok", "grok-build"}, Harness: "grok",
			},
			"kimi": {
				Aliases: []string{"kimi", "kimi-code"}, Harness: "kimi",
			},
		},
		Projects: map[string]Project{
			"workspace": {Home: ProjectHome{Path: "~/steve-workspace"}},
		},
		Harnesses: map[string]Harness{
			"codex": {
				Adapter: "codex-acp", Permission: PermissionRead,
			},
			"claude-code": {
				Adapter: "claude-agent-acp", Permission: PermissionRead,
			},
			"grok": {
				Command: "grok", Args: []string{"agent", "--no-leader", "stdio"}, Permission: PermissionRead,
			},
			"kimi": {
				Command: "kimi", Args: []string{"acp"}, Permission: PermissionRead,
			},
		},
		MCPServers: map[string]MCPServer{},
		Feishu:     feishu,
		Gateway: Gateway{
			PromptTimeout: Duration(10 * time.Minute), StatePath: DefaultStatePath,
		},
	}
}

// CommittedError means the replacement is visible, but syncing its directory
// failed. Callers must retain the new configuration; rolling back only their
// in-memory state would disagree with the file already installed by rename.
type CommittedError struct{ Err error }

func (e *CommittedError) Error() string {
	return "configuration applied; directory sync failed (durability uncertain): " + e.Err.Error()
}
func (e *CommittedError) Unwrap() error { return e.Err }

func Committed(err error) bool {
	var committed *CommittedError
	return errors.As(err, &committed)
}

func Save(path string, cfg *Config) error {
	return saveWithSync(path, cfg, fsx.SyncDir)
}

func saveWithSync(path string, cfg *Config, syncParent func(string) error) error {
	persisted := *cfg
	persisted.Harnesses = maps.Clone(cfg.Harnesses)
	for name, h := range persisted.Harnesses {
		if h.Adapter != "" {
			h.Command = ""
			persisted.Harnesses[name] = h
		}
	}
	data, err := json.MarshalIndent(&persisted, "", "  ")
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
	if err := cfg.CheckFileRevision(path); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace config: %w", err)
	}
	cfg.rememberFileRevision(path, append(data, '\n'))
	if err := syncParent(dir); err != nil {
		return &CommittedError{Err: err}
	}
	return nil
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
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}

// Load reads a configuration file and returns it ready to run: the file's
// values with the defaults filled in, checked, and its paths resolved.
func Load(path string) (*Config, error) {
	cfg, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validateSettings(); err != nil {
		return nil, err
	}
	if len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("projects{} is required: at least one project with a home path")
	}
	cfg.inferDefaultProject()
	if err := cfg.validateTopology(); err != nil {
		return nil, err
	}
	cfg.resolvePaths()
	return cfg, nil
}

// readConfigFile decodes exactly one JSON object with no unknown fields and
// remembers the file's revision for the optimistic check Save makes.
func readConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	cfg.rememberFileRevision(path, data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse config: expected one JSON object")
	}
	return cfg, nil
}

// applyDefaults fills what the file left out and trims the identities it
// gave. Nothing here can fail; the checks come after.
func (c *Config) applyDefaults() {
	c.Policies = c.Policies.WithDefaults()
	c.Gateway.OwnerID = strings.TrimSpace(c.Gateway.OwnerID)
	if c.Gateway.DefaultChannel == "" {
		c.Gateway.DefaultChannel = "console"
		if c.FeishuEnabled() {
			c.Gateway.DefaultChannel = "feishu"
		}
	}
	c.Feishu.applyDefaults()
	c.Feishu.OwnerOpenID = strings.TrimSpace(c.Feishu.OwnerOpenID)
	if c.Gateway.PromptTimeout <= 0 {
		c.Gateway.PromptTimeout = Duration(10 * time.Minute)
	}
	if c.Gateway.RecoveryQuiet <= 0 {
		c.Gateway.RecoveryQuiet = Duration(DefaultRecoveryQuiet)
	}
	if c.Gateway.OfflineReminderAfter == 0 {
		c.Gateway.OfflineReminderAfter = Duration(DefaultOfflineReminder)
	}
	if c.Gateway.StatePath == "" {
		c.Gateway.StatePath = DefaultStatePath
	}
	if c.Gateway.ReadModelAddr == "" {
		c.Gateway.ReadModelAddr = "127.0.0.1:7710"
	}
	for id, item := range c.Harnesses {
		if item.Permission == "" {
			item.Permission = PermissionRead
			c.Harnesses[id] = item
		}
	}
}

// inferDefaultProject makes a lone project the default when the file names
// none: with one project there is nothing else a conversation could mean.
func (c *Config) inferDefaultProject() {
	c.Gateway.DefaultProject = DefaultProjectID(c.Gateway.DefaultProject, c.Projects)
}

// LocalHomeNode reports whether a project home's node is this
// application's own machine. A bootstrap configuration leaves the node
// empty; a shared declaration names it, using the hub's node identity.
func (c *Config) LocalHomeNode(node string) bool {
	if node == "" || node == c.Gateway.HubID {
		return true
	}
	return c.RuntimeHome != nil && node == c.RuntimeHome.Node
}

// LocalWorkspaceRoot is this machine's workspace: where it keeps the work
// it holds, projects included. It is derived from this machine's own state
// directory, never from a declaration a shared configuration carries,
// which names directories on whichever machine wrote it.
func (c *Config) LocalWorkspaceRoot() string {
	if root := strings.TrimSpace(c.Gateway.WorkspaceRoot); root != "" {
		return root
	}
	return filepath.Dir(c.Gateway.StatePath)
}

// DefaultProjectID is the project a conversation starts in: the preferred
// one when it is declared, otherwise the only one there is, otherwise none.
func DefaultProjectID(preferred string, projects map[string]Project) string {
	if _, ok := projects[preferred]; ok {
		return preferred
	}
	if len(projects) == 1 {
		for id := range projects {
			return id
		}
	}
	return ""
}

// validateSettings checks the fields that stand on their own, before the
// projects are known.
func (c *Config) validateSettings() error {
	if err := c.Policies.Validate(); err != nil {
		return err
	}
	if c.Gateway.Locale != "" && c.Gateway.Locale != "zh" && c.Gateway.Locale != "en" {
		return fmt.Errorf("gateway.locale must be zh or en")
	}
	return nil
}

// validateTopology checks that projects, nodes, harnesses and agents refer
// to each other consistently. The checks keep their historical order so a
// file with several faults reports the same first one it always did.
func (c *Config) validateTopology() error {
	if err := c.validateProjects(); err != nil {
		return err
	}
	if err := c.validateNodePlacement(); err != nil {
		return err
	}
	if c.Gateway.Level != "" && !datalevel.Level(c.Gateway.Level).Valid() {
		return fmt.Errorf("gateway.level %q is not public, internal, restricted or sealed", c.Gateway.Level)
	}
	if c.Gateway.DefaultProject != "" {
		if _, ok := c.Projects[c.Gateway.DefaultProject]; !ok {
			return fmt.Errorf("gateway.default_project %q is not in projects{}", c.Gateway.DefaultProject)
		}
	}
	if err := c.validateHarnessDeclarations(); err != nil {
		return err
	}
	if c.Gateway.Planner != "" {
		if _, ok := c.Agents[c.Gateway.Planner]; !ok {
			return fmt.Errorf("gateway.planner references unknown agent %q", c.Gateway.Planner)
		}
	}
	if err := c.validateNodeEndpoints(); err != nil {
		return err
	}
	if _, err := c.AgentCatalog(); err != nil {
		return err
	}
	// Not configbuild.HarnessManager: a harness that names an adapter has
	// no command until the adapter is fetched, and loading a file must not
	// depend on the network. Loading checks the configuration; the manager
	// checks that it can be run.
	if err := c.validateHarnesses(); err != nil {
		return err
	}
	if err := c.validateAgents(); err != nil {
		return err
	}
	return c.ValidatePlugins()
}

func (c *Config) validateProjects() error {
	for id, item := range c.Projects {
		if id == ReservedHomeProject {
			return fmt.Errorf("project id %q is reserved for Steve's own home directory", id)
		}
		if item.Home.Path == "" {
			return fmt.Errorf("project %q home.path is required", id)
		}
		if item.Home.Node != "" {
			if _, ok := c.Nodes[item.Home.Node]; !ok {
				return fmt.Errorf("project %q home.node %q is not in nodes{}", id, item.Home.Node)
			}
		}
	}
	return nil
}

// validateNodePlacement checks each node's region and level: what the hub
// may place there.
func (c *Config) validateNodePlacement() error {
	for id, item := range c.Nodes {
		if item.Region != "" && item.Region != c.Gateway.Region && c.Gateway.Region != "" || item.Region != "" && c.Gateway.Region == "" && item.Region != "default" {
			if _, ok := c.Gateway.Regions[item.Region]; !ok {
				return fmt.Errorf("node %q is in region %q, which gateway.regions does not list", id, item.Region)
			}
		}
		if item.Level != "" && !datalevel.Level(item.Level).Valid() {
			return fmt.Errorf("node %q level %q is not public, internal, restricted or sealed", id, item.Level)
		}
	}
	return nil
}

// validateNodeEndpoints checks each node says how to reach it and how to
// prove we may.
func (c *Config) validateNodeEndpoints() error {
	for id, item := range c.Nodes {
		if strings.TrimSpace(item.Addr) == "" {
			return fmt.Errorf("node %q addr is required", id)
		}
		if strings.TrimSpace(item.Token) == "" {
			return fmt.Errorf("node %q token is required", id)
		}
	}
	return nil
}

// validateHarnessDeclarations checks each harness names either a catalog
// adapter or a command of its own, never both or neither.
func (c *Config) validateHarnessDeclarations() error {
	for id, item := range c.Harnesses {
		switch {
		case item.Adapter != "" && item.Command != "":
			return fmt.Errorf("harness %q sets both adapter and command; pick one", id)
		case item.Adapter == "" && item.Command == "":
			return fmt.Errorf("harness %q needs an adapter or a command", id)
		case item.Adapter != "":
			if _, known := adapter.Catalog[item.Adapter]; !known {
				return fmt.Errorf("harness %q: adapter %q is not one of %s", id, item.Adapter, strings.Join(adapter.Names(), ", "))
			}
			if len(item.Args) > 0 {
				return fmt.Errorf("harness %q: an adapter takes no args", id)
			}
		}
	}
	return nil
}

// validateAgents checks each agent's harness, node and MCP servers exist
// where they have to: on the hub for a hub-local agent, on the node for one
// placed there.
func (c *Config) validateAgents() error {
	for id, item := range c.Agents {
		if _, ok := c.Harnesses[item.Harness]; !ok && item.Node == "" {
			return fmt.Errorf("agent %q references unknown harness %q", id, item.Harness)
		}
		if item.Node != "" {
			if _, ok := c.Nodes[item.Node]; !ok {
				return fmt.Errorf("agent %q references unknown node %q", id, item.Node)
			}
		}
		for _, server := range item.MCPServers {
			// An agent on another machine uses that machine's servers,
			// bound there at admission; only a hub-local agent's names must
			// exist in this configuration.
			if _, ok := c.MCPServers[server]; !ok && item.Node == "" {
				return fmt.Errorf("agent %q references unknown MCP server %q", id, server)
			}
		}
	}
	return nil
}

// resolvePaths makes every hub-local path absolute and derives the home
// directory from the state path when the file gave none. A project home on
// another node is that node's path and is left alone: resolving it against
// the hub's filesystem would produce a path that means something different,
// or nothing, over there.
func (c *Config) resolvePaths() {
	c.Gateway.StatePath = absolute(c.Gateway.StatePath)
	if c.Gateway.HomePath == "" {
		c.Gateway.HomePath = filepath.Join(filepath.Dir(c.Gateway.StatePath), "home")
	}
	c.Gateway.HomePath = absolute(c.Gateway.HomePath)
	if c.Gateway.WorkspaceRoot != "" {
		c.Gateway.WorkspaceRoot = absolute(c.Gateway.WorkspaceRoot)
	}
	for id, item := range c.Agents {
		for i, skill := range item.Skills {
			item.Skills[i] = absolute(skill)
		}
		c.Agents[id] = item
	}
	for id, item := range c.Projects {
		if item.Home.Node == "" {
			item.Home.Path = absolute(item.Home.Path)
		}
		for i, skill := range item.Skills {
			item.Skills[i] = absolute(skill)
		}
		c.Projects[id] = item
	}
	for id, item := range c.Harnesses {
		item.ProcessDir = absolute(item.ProcessDir)
		c.Harnesses[id] = item
	}
}

// AgentCatalog builds the in-memory catalog the agents{} section declares.
// Load builds it too, so an alias clash or a missing default agent is
// refused with the file rather than at first use.
func (c *Config) AgentCatalog() (*agent.Catalog, error) {
	configs := make(map[string]agent.Config, len(c.Agents))
	for id, item := range c.Agents {
		if item.PluginOrigin != nil {
			if err := item.PluginOrigin.Adopted.CheckRemaining(item.Skills, item.MCPServers); err != nil {
				return nil, fmt.Errorf("agent %s: %w", id, err)
			}
		}
		configs[id] = agent.Config{
			PluginOrigin: item.PluginOrigin.Clone(),
			Approval:     c.Gateway.DefaultApproval,
			Harness:      item.Harness, Node: item.Node, Model: item.Model, Options: item.Options, About: item.About, Requires: item.Requires,
			Aliases:      item.Aliases,
			SystemPrompt: item.SystemPrompt, Skills: item.Skills, MCPServers: item.MCPServers, Default: item.Default,
		}
	}
	return agent.NewCatalog(configs)
}

func (c *Config) validateHarnesses() error {
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

// AdapterDir is where fetched adapters live: beside the ledger, because
// they are part of this deployment's state, not of anyone's home.
func (c *Config) AdapterDir() string {
	return filepath.Join(filepath.Dir(absolute(c.Gateway.StatePath)), "adapters")
}

// DefaultStatePath is where a Hub keeps its state unless configured otherwise.
const DefaultStatePath = "~/.steve/state.json"

// StateDir is the directory a gateway.state_path names, resolved as Load
// resolves it: "~" against this user's home and a relative path against the
// working directory. Clients use it to find what the Hub keeps there without
// loading the whole configuration.
func StateDir(statePath string) string {
	if statePath == "" {
		statePath = DefaultStatePath
	}
	return filepath.Dir(absolute(statePath))
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
func (c *Config) NodeLevels() map[string]datalevel.Level {
	out := map[string]datalevel.Level{}
	for id, n := range c.Nodes {
		out[id] = datalevel.Level(n.Level).OrDefault()
	}
	return out
}

// HubLevel is the hub machine's data level: what was configured, or else
// the highest level of anything the hub is the durable place for — every
// project except a sealed one homed on a node, and Steve's own home, which
// is restricted. A hub that could not hold what it stores would refuse
// its own projects at the first turn.
func (c *Config) HubLevel() datalevel.Level {
	if c.Gateway.Level != "" {
		return datalevel.Level(c.Gateway.Level)
	}
	level := datalevel.Restricted
	for _, p := range c.Projects {
		candidate := datalevel.Level(p.Level).OrDefault()
		if candidate == datalevel.Sealed && p.Home.Node != "" {
			continue
		}
		if !candidate.Admits(level) {
			level = candidate
		}
	}
	return level
}

// NodeRegions is the region of each node; unset means the hub's own.
func (c *Config) NodeRegions() map[string]string {
	out := map[string]string{}
	for id, n := range c.Nodes {
		out[id] = n.Region
	}
	return out
}

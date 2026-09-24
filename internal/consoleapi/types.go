// Package consoleapi defines the data and service ports shared by console
// applications and their transport adapters. It performs no I/O.
package consoleapi

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Console is what the page needs to act, not only to watch: send a line as
// the owner into a conversation and read what came back. The token that
// guards the read model is the owner's credential here; without a
// console wired, the endpoints answer that acting is off.
// QuoteRef points at one stored line of a thread to carry with a message.
type QuoteRef struct {
	Conversation string `json:"conversation"`
	ReplyID      string `json:"reply_id"`
}

// Selectors are what an agent's harness offers to choose from, and
// what is set: the model, and every other selector by option id.
type Selectors struct {
	Model   string        `json:"model,omitempty"`
	Models  []view.Choice `json:"models,omitempty"`
	Options []view.Option `json:"options,omitempty"`
	// Preferred is what the owner chose for this agent in this thread.
	Preferred map[string]string `json:"preferred,omitempty"`
}

// ConversationInitializer creates a project-bound conversation without a message.
type ConversationInitializer interface {
	InitializeConversation(ctx context.Context, conversation, project string) error
}

type Console interface {
	Send(ctx context.Context, conversation, input string) (Reply, error)
	// SendCommand is Send with an idempotency key from the page.
	SendCommand(ctx context.Context, conversation, input, commandID string) (Reply, error)
	// SendCommandWith carries quotes of other lines along with the input.
	SendCommandWith(ctx context.Context, conversation, input, commandID string, quotes []QuoteRef) (Reply, error)
	Enqueue(ctx context.Context, conversation, input string, quotes []QuoteRef) (Exchange, error)
	// EnqueueCommand durably accepts or replays one conversation/client key.
	EnqueueCommand(ctx context.Context, conversation, input, commandID string, quotes []QuoteRef) (Exchange, error)
	Queue(conversation string) []Exchange
	DeleteQueued(id string) error
	EditQueued(id, input string) (Exchange, error)
	Steer(ctx context.Context, id string) (Exchange, error)
	Replies(conversation string) []Reply
	// Conversations names every console conversation with a transcript;
	// Summaries describes each one for a sidebar.
	Conversations() []string
	Summaries(ctx context.Context) []Conversation
	Update(ctx context.Context, conversation string, patch ConversationPatch) error
	// Context is where a conversation stands; Verbs is what it can be told.
	Context(ctx context.Context, conversation string) (Context, error)
	// Setup is what the conversation's agent works with: the assembled
	// instructions and the pieces they are made of.
	Setup(ctx context.Context, conversation, agent string) (Setup, error)
	Verbs() []Verb
	// Suggest completes a line the page is typing, by the coordinator's
	// rules: verbs, agents, projects, this conversation's tasks.
	Suggest(ctx context.Context, conversation, line string) []Suggestion
}

// Suggestion is one completion for the line being typed.
type Suggestion struct {
	Label  string `json:"label"`
	Args   string `json:"args,omitempty"`
	Detail string `json:"detail,omitempty"`
	Insert string `json:"insert"`
	Muted  bool   `json:"muted,omitempty"`
}

// Context is a conversation's standing for the page's context bar: its
// project, its current agent, and every agent as a candidate with the
// reason it can or cannot take the next line.
type Context struct {
	Conversation string          `json:"conversation"`
	Project      *ContextProject `json:"project,omitempty"`
	Agent        *AgentChoice    `json:"agent,omitempty"`
	Agents       []AgentChoice   `json:"agents"`
}

type ContextProject struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Level   string `json:"level"`
	Repo    string `json:"repo"`
	Version int64  `json:"version"`
	Bound   bool   `json:"bound"`
}

type AgentChoice struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
	Ready   bool   `json:"ready"`
	Why     string `json:"why,omitempty"`
	Usable  bool   `json:"usable"`
	Because string `json:"because,omitempty"`
	Current bool   `json:"current,omitempty"`
	// Place is where this agent would work in the conversation's project:
	// the project's own rule, answered by the server, so no page has to
	// know it. Nil when the agent's machine has no workspace of it.
	Place *Placement `json:"place,omitempty"`
}

// Placement is one workspace as a place to work: which, what kind, where.
type Placement struct {
	Workspace string `json:"workspace"`
	Kind      string `json:"kind"`
	Node      string `json:"node"`
}

// Verb is one console verb with its argument shape and a line of help.
type Verb struct {
	Command string `json:"command"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary"`
}

// Reply is one exchange on the console.
// AddNodeRequest is the page adding a machine: a name, where the hub
// dials it, and the data level it may handle. HubURL is where the machine
// will fetch its bootstrap from, as the page reached the hub.
type AddNodeRequest struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Level  string `json:"level,omitempty"`
	HubURL string `json:"-"`
}

// AddNodeResult is what to run on the machine: one command that writes
// its config, fetches the binary if the hub has one, and starts it.
type AddNodeResult struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Command string `json:"command"`
	Note    string `json:"note,omitempty"`
}

// AgentSpec is the editable part of an agent: where it runs, with what,
// which model it prefers, what its machine must offer, which MCP
// servers it uses.
type AgentSpec struct {
	Harness    string            `json:"harness"`
	Node       string            `json:"node,omitempty"`
	Model      string            `json:"model,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	About      string            `json:"about,omitempty"`
	Requires   []string          `json:"requires"`
	MCPServers []string          `json:"mcp_servers"`
}

// AddProjectRequest is the page declaring a project: a name, the directory
// it lives in under the coordinator's workspace, how agents may change it,
// its data level. Node names the coordinator, which is where a project's
// canonical directory is made; other machines get a copy of their own.
type AddProjectRequest struct {
	ID    string `json:"id"`
	Node  string `json:"node,omitempty"`
	Path  string `json:"path"`
	Repo  string `json:"repo,omitempty"`
	Level string `json:"level,omitempty"`
}

// ProjectHomeRequest moves a project's canonical directory on the machine
// that holds it.
type ProjectHomeRequest struct {
	Path string `json:"path"`
}

// AddWorkspaceRequest is the page giving a project a copy on a machine:
// "adopt" a directory already there, or "clone" the project into one.
// The directory is not asked for: a copy uses the project's own directory
// name under the chosen machine's workspace, so every machine agrees.
type AddWorkspaceRequest struct {
	Node   string `json:"node,omitempty"`
	Origin string `json:"origin,omitempty"`
}

// ResolveConflictsResult is what asking for a project's stuck landings to
// be resolved started. The work runs as a plan and takes minutes, so the
// page is told how many were picked up, not how they turned out.
type ResolveConflictsResult struct {
	Started int      `json:"started"`
	Skipped []string `json:"skipped,omitempty"`
}

// ConflictFileView is one file of a merge conflict as git left it: both
// sides with the markers between them, which is the text a person edits
// to resolve it.
type ConflictFileView struct {
	Artifact  string `json:"artifact"`
	Project   string `json:"project"`
	Path      string `json:"path"`
	Text      string `json:"text"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ApprovalSync is what syncing the fleet to the default approval stance
// did: which agents were let go of their own pinned mode, which already
// followed the default, and which run an AI tool that offers no mode at
// that level — where the default cannot land and the owner should know.
type ApprovalSync struct {
	Intent    string              `json:"intent"`
	Cleared   []ApprovalSyncAgent `json:"cleared,omitempty"`
	Following []string            `json:"following,omitempty"`
	Unmapped  []string            `json:"unmapped,omitempty"`
}

// ApprovalSyncAgent names one agent and the mode it used to pin.
type ApprovalSyncAgent struct {
	Agent string `json:"agent"`
	Was   string `json:"was,omitempty"`
}

// AddAgentRequest is the page adding an agent: an id, the AI tool it
// runs, the machine it runs on ("" is the hub), a preferred model.
type AddAgentRequest struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Node    string `json:"node,omitempty"`
	Model   string `json:"model,omitempty"`
	// About says what the agent is good for, so planning can pick it.
	About string `json:"about,omitempty"`
	// Default makes this the agent a conversation starts with. The first
	// agent registered holds that place even when this is not asked for.
	Default bool `json:"default,omitempty"`
}

// Admin changes the fleet at runtime and persists the change: the page
// adds machines and agents without a restart.
type Admin interface {
	AddNode(ctx context.Context, req AddNodeRequest) (AddNodeResult, error)
	// RemoveNode forgets a machine: nothing may still live on it.
	RemoveNode(ctx context.Context, name string) error
	AddAgent(ctx context.Context, req AddAgentRequest) error
	// Bootstrap is the script a machine runs, given its own token.
	Bootstrap(name, token string) (string, bool)
	// NodeBinary is the steve-node executable to hand a machine presenting
	// a node token, if the hub has one.
	NodeBinary(token string) (string, bool)
	// UpdateAgent replaces an agent's placement, model and conditions;
	// RemoveAgent forgets it. Both persist to the config file.
	UpdateAgent(ctx context.Context, id string, spec AgentSpec) error
	RemoveAgent(ctx context.Context, id string) error
	// SyncAgentApproval drops every agent's own approval mode so the whole
	// fleet follows the hub's default approval stance.
	SyncAgentApproval(ctx context.Context) (ApprovalSync, error)
	// AddProject declares a project and records it in the config file;
	// RemoveProject retires one and drops it from the file.
	AddProject(ctx context.Context, req AddProjectRequest) error
	RemoveProject(ctx context.Context, id string) error
	// DeleteConversation removes a thread and what only it held: its
	// agent sessions, its tasks, its schedules and its transcript.
	DeleteConversation(ctx context.Context, conversation string) error
	// SetProjectHome moves a project's canonical directory.
	SetProjectHome(ctx context.Context, id, path string) error
	// AddWorkspace gives a project a copy on a machine — a directory that
	// is there, or one cloned into place; RemoveWorkspace forgets one.
	AddWorkspace(ctx context.Context, projectID string, req AddWorkspaceRequest) error
	RemoveWorkspace(ctx context.Context, projectID, node string) error
	// ResolveConflicts hands the project's landings that stopped at a merge
	// conflict to an agent, in the background.
	ResolveConflicts(ctx context.Context, projectID string) (ResolveConflictsResult, error)
	// ResolveAllConflicts covers every project at once, and
	// ResolveConflictWithAgent one conflict named by the artifact behind
	// it. ConflictFile reads a conflicted file as git left it, and
	// ResolveConflictByHand lands a person's own resolution of it.
	// RetryConflict lands a result that stopped with no merge to work on
	// again, once its owner has dealt with what stopped it; landing is the
	// stop the owner saw, and a result stuck on another is left alone.
	ResolveAllConflicts(ctx context.Context) (ResolveConflictsResult, error)
	ResolveConflictWithAgent(ctx context.Context, artifactID string) (ResolveConflictsResult, error)
	ConflictFile(ctx context.Context, artifactID, path string) (ConflictFileView, error)
	ResolveConflictByHand(ctx context.Context, artifactID string, edits []artifact.Edit) error
	RetryConflict(ctx context.Context, artifactID, landing string) error
	// NodeSettings reads what a machine offers; SetNodeSettings rewrites
	// it and answers what is in force. The hub machine is one of them.
	NodeSettings(ctx context.Context, name string) (nodewire.Settings, error)
	SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error)
	// Skills is what the hub can hand its agents and what it does hand
	// them; SetSkill turns one on or off, the paths are where skills are
	// looked for, SkillContent is one skill's SKILL.md.
	Skills(ctx context.Context) (SkillsView, error)
	SetSkill(ctx context.Context, name string, enabled bool) error
	AddSkillPath(ctx context.Context, path string) error
	RemoveSkillPath(ctx context.Context, path string) error
	SkillContent(ctx context.Context, name string) (SkillDoc, error)
	// AddSkillSource installs a git repository of skills; UpdateSkillSources
	// fetches them all again; RemoveSkillSource forgets one.
	AddSkillSource(ctx context.Context, spec string) (SkillSource, error)
	UpdateSkillSources(ctx context.Context) ([]SkillSource, error)
	RemoveSkillSource(ctx context.Context, slug string) error
	// MachineSkills lists the skills each machine's AI tools already
	// have, outside Steve; ImportSkill loads one onto the hub.
	MachineSkills(ctx context.Context) []MachineSkills
	RefreshMachineSkills(ctx context.Context) []MachineSkills
	ImportSkill(ctx context.Context, node, path string) (string, error)
	// MCP is the MCP page; ProbeMCP asks a machine what tools one of its
	// servers offers; AdoptMCP copies a coding agent's own server into a
	// machine's settings on that machine; RemoveMCP drops one;
	// SearchMCPRegistry asks the registry; InstallMCP puts an entry on
	// a machine.
	MCP(ctx context.Context) (MCPView, error)
	ProbeMCP(ctx context.Context, node, name string) (MCPProbeView, error)
	AdoptMCP(ctx context.Context, node, source, name string) error
	RemoveMCP(ctx context.Context, node, name string) error
	SearchMCPRegistry(ctx context.Context, q string) ([]MCPRegistryEntry, error)
	InstallMCP(ctx context.Context, req InstallMCPRequest) error
	// Home is Steve's own three files — who it is, who the owner is,
	// what it remembers; SetHomeFile rewrites one.
	Home(ctx context.Context) (HomeView, error)
	SetHomeFile(ctx context.Context, name, text string) error
	// SetProjectMemory rewrites one project's memory whole.
	SetProjectMemory(ctx context.Context, project, text string) error
	// AttemptChanges is the files an attempt changed; AttemptDiff one of
	// them. Owner-only, like the rest of the console.
	AttemptChanges(ctx context.Context, attempt string) (ChangeIndex, error)
	AttemptDiff(ctx context.Context, attempt, path string) (FileDiff, error)
	// Selectors reads what an agent offers using a temporary discovery
	// session; SetPreferences records choices and applies them to the
	// session answering right now, or rolls the session over so the next
	// turn honours them. It reports true when the live session took them.
	Selectors(ctx context.Context, conversation, agent string) (Selectors, error)
	SetPreferences(ctx context.Context, conversation, agent string, patch map[string]string) (bool, error)
	SetTaskMeta(ctx context.Context, task string, patch TaskMetaPatch) error
	// AttemptTree lists a directory of an attempt's snapshot; AttemptFile
	// reads one file of it. Both bounded, both owner-only.
	AttemptTree(ctx context.Context, attempt, dir string) (TreeView, error)
	AttemptFile(ctx context.Context, attempt, path string) (FileView, error)
}

// SkillsView is the skills page: where skills are looked for, every
// skill found there with whether it is handed to agents, and whether
// each machine holds the current bundle.
type SkillsView struct {
	Plugins     []PluginResourceView `json:"plugins,omitempty"`
	Fingerprint string               `json:"fingerprint"`
	SearchPaths []string             `json:"search_paths"`
	// BuiltinRoot is the directory the skills shipped with steve are
	// written to; it is searched last and cannot be removed.
	BuiltinRoot string      `json:"builtin_root,omitempty"`
	Skills      []SkillView `json:"skills"`
	Nodes       []SkillNode `json:"nodes"`
	// Sources are the git repositories skills were installed from.
	Sources []SkillSource `json:"sources"`
}

// SkillSource is one installed repository: where it came from, what
// was fetched, and the skills it lists.
type SkillSource struct {
	Slug      string    `json:"slug"`
	URL       string    `json:"url"`
	Ref       string    `json:"ref,omitempty"`
	Subdir    string    `json:"subdir,omitempty"`
	Root      string    `json:"root"`
	Head      string    `json:"head,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Skills    []string  `json:"skills"`
	Error     string    `json:"error,omitempty"`
}

// SkillView is one skill: named by its directory, described by its
// SKILL.md, and pinned by agents or projects that ask for it by path.
type SkillView struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Root        string `json:"root"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Builtin     bool   `json:"builtin,omitempty"`
	// Source names the installed repository a skill came from, if one.
	Source   string   `json:"source,omitempty"`
	Agents   []string `json:"agents"`
	Projects []string `json:"projects"`
}

// SkillNode says whether a machine holds the bundle the hub last packed.
type SkillNode struct {
	Name   string `json:"name"`
	Up     bool   `json:"up"`
	Synced bool   `json:"synced"`
	Takes  bool   `json:"takes"`
}

// MachineSkills is what one machine's AI tools have of their own.
type MachineSkills struct {
	Name   string       `json:"name"`
	Hub    bool         `json:"hub,omitempty"`
	Up     bool         `json:"up"`
	Skills []FoundSkill `json:"skills"`
	Error  string       `json:"error,omitempty"`
}

// FoundSkill is one of them.
type FoundSkill struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Loaded says the hub already has a skill of this name.
	Loaded bool `json:"loaded,omitempty"`
}

// MCPView is the MCP page: every deployment on every machine, the
// platform's own session servers, and what each machine's coding
// agents configured themselves.
type MCPView struct {
	Plugins     []PluginResourceView `json:"plugins,omitempty"`
	Deployments []MCPDeployment      `json:"deployments"`
	Platform    []MCPPlatform        `json:"platform"`
	Machines    []MCPMachine         `json:"machines"`
}

// MCPDeployment is one server on one machine: the shape (values of env
// and headers stay on the machine), who attaches, whether the command
// resolves, and the last probe.
type MCPDeployment struct {
	Node       string   `json:"node"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	Agents     []string `json:"agents"`
	// Resolvable is what the machine last said about the command being
	// on its PATH (nil when it has not said).
	Resolvable *bool  `json:"resolvable,omitempty"`
	Provenance string `json:"provenance,omitempty"`
	// SameNameElsewhere flags a name another machine also has: two
	// deployments, one name — the same service only if the owner says.
	SameNameElsewhere bool          `json:"same_name_elsewhere,omitempty"`
	Probe             *MCPProbeView `json:"probe,omitempty"`
}

// MCPProbeView is the last probe of a deployment as the hub remembers
// it. Stale says the machine's configuration changed since.
type MCPProbeView struct {
	At            time.Time          `json:"at"`
	OK            bool               `json:"ok"`
	Error         string             `json:"error,omitempty"`
	Stale         bool               `json:"stale,omitempty"`
	Tools         []nodewire.MCPTool `json:"tools"`
	Digest        string             `json:"digest,omitempty"`
	ServerName    string             `json:"server_name,omitempty"`
	ServerVersion string             `json:"server_version,omitempty"`
	Protocol      string             `json:"protocol,omitempty"`
}

// MCPPlatform is a server the hub makes per session.
type MCPPlatform struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Tools       []PlatformTool `json:"tools"`
}

// PlatformTool is one of its tools.
type PlatformTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MCPMachine is what one machine's coding agents configured themselves.
type MCPMachine struct {
	Name        string   `json:"name"`
	Hub         bool     `json:"hub,omitempty"`
	Up          bool     `json:"up"`
	Unsupported bool     `json:"unsupported,omitempty"`
	Own         []MCPOwn `json:"own"`
}

// MCPOwn is one of those; Adopted says the machine's settings already
// have a server of this name.
type MCPOwn struct {
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	Scope      string   `json:"scope,omitempty"`
	Type       string   `json:"type"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	Adopted    bool     `json:"adopted,omitempty"`
}

// MCPRegistryEntry is one registry entry as the page shows it.
type MCPRegistryEntry struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Version     string               `json:"version,omitempty"`
	Repository  string               `json:"repository,omitempty"`
	Packages    []MCPRegistryPackage `json:"packages"`
	Remotes     []MCPRegistryRemote  `json:"remotes"`
}

type MCPRegistryPackage struct {
	RegistryType string           `json:"registry_type"`
	Identifier   string           `json:"identifier"`
	Version      string           `json:"version,omitempty"`
	RuntimeHint  string           `json:"runtime_hint,omitempty"`
	Transport    string           `json:"transport,omitempty"`
	Needs        string           `json:"needs,omitempty"`
	Env          []MCPRegistryEnv `json:"env"`
}

type MCPRegistryRemote struct {
	Type    string           `json:"type"`
	URL     string           `json:"url"`
	Headers []MCPRegistryEnv `json:"headers"`
}

type MCPRegistryEnv struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Default     string `json:"default,omitempty"`
}

// InstallMCPRequest is the page installing a registry entry: which
// entry, which of its packages or remotes, onto which machine, under
// what name, with the values the entry asked for.
type InstallMCPRequest struct {
	Node    string            `json:"node"`
	Name    string            `json:"name"`
	Entry   string            `json:"entry"`
	Package *int              `json:"package,omitempty"`
	Remote  *int              `json:"remote,omitempty"`
	Values  map[string]string `json:"values"`
}

// SkillDoc is one skill's SKILL.md.
type SkillDoc struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// HomeView is Steve's home as a page shows it: the directory, the three
// files with how much room each has, and how much of it reaches the
// agent in the owner's private chat and in front of anyone else.
type HomeView struct {
	Path        string     `json:"path"`
	Files       []HomeFile `json:"files"`
	TotalBudget int        `json:"total_budget"`
	OwnerBytes  int        `json:"owner_bytes"`
	GuestBytes  int        `json:"guest_bytes"`
	Warnings    []string   `json:"warnings"`
	// Projects are each project's own memory, home project excluded.
	Projects []ProjectMemory `json:"projects"`
	// Audit is where memory writes are logged.
	Audit string `json:"audit,omitempty"`
}

// AttemptView is one attempt of a task as the page lists it: what ran
// where, on which snapshots, and how it ended.
type AttemptView struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Agent     string    `json:"agent,omitempty"`
	Node      string    `json:"node,omitempty"`
	Harness   string    `json:"harness,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Base      string    `json:"base,omitempty"`
	Artifact  string    `json:"artifact,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// Files is how many paths the attempt's snapshots differ in.
	Files int `json:"files,omitempty"`
}

// TreeView is one directory of an attempt's snapshot.
type TreeView struct {
	Attempt   string          `json:"attempt"`
	Commit    string          `json:"commit"`
	Which     string          `json:"which"` // result | base
	Dir       string          `json:"dir"`
	Entries   []gitrepo.Entry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// FileView is one file of an attempt's snapshot.
type FileView struct {
	Attempt   string `json:"attempt"`
	Commit    string `json:"commit"`
	Path      string `json:"path"`
	Text      string `json:"text"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ChangeSummary is what a turn changed, as the reply keeps it: the
// attempt to ask for the index and its bounded totals. Nil line/binary totals
// mean unknown. Truncated totals cover the indexed files only. Paths and
// diffs are read on demand, never stored with the reply.
type ChangeSummary struct {
	Attempt     string `json:"attempt"`
	Project     string `json:"project,omitempty"`
	Base        string `json:"base,omitempty"`
	Artifact    string `json:"artifact,omitempty"`
	Files       int    `json:"files"`
	Added       *int   `json:"added,omitempty"`
	Deleted     *int   `json:"deleted,omitempty"`
	BinaryFiles *int   `json:"binary_files,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Note        string `json:"note,omitempty"`
}

// ChangeIndex is the files an attempt changed, bounded.
type ChangeIndex struct {
	Attempt   string           `json:"attempt"`
	Project   string           `json:"project,omitempty"`
	Base      string           `json:"base,omitempty"`
	Artifact  string           `json:"artifact,omitempty"`
	Changes   []gitrepo.Change `json:"changes"`
	Truncated bool             `json:"truncated,omitempty"`
	Note      string           `json:"note,omitempty"`
}

// FileDiff is one changed file's unified diff, cut at the store's limit.
type FileDiff struct {
	Path      string `json:"path"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ProjectMemory is one project's memory as the page edits it.
type ProjectMemory struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Text   string `json:"text"`
	Bytes  int    `json:"bytes"`
	Budget int    `json:"budget"`
	Facts  int    `json:"facts"`
}

// HomeFile is one of the three: what it is for is the page's to say.
type HomeFile struct {
	Name     string `json:"name"`
	Text     string `json:"text"`
	Bytes    int    `json:"bytes"`
	Budget   int    `json:"budget"`
	Template bool   `json:"template,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
}

// Conversation is one console thread as the sidebar lists it: named by
// its first line, placed by its project and agent, and marked while a
// line of it runs.
type Conversation struct {
	ID string `json:"id"`
	// Transport owns the opaque ID. Channel histories have no Console commands.
	Transport string `json:"transport,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	// Execution is channel processing, not task lifetime: running, idle, unknown.
	Execution string `json:"execution,omitempty"`
	// Title stays empty until non-command input or an explicit name supplies it.
	// Clients display a localized "New conversation" without persisting that placeholder.
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// Place is where the conversation's agent works in its project now.
	Place   *Placement `json:"place,omitempty"`
	LastAt  time.Time  `json:"last_at"`
	Count   int        `json:"count"`
	Running bool       `json:"running"`
	// TitleBy says who named it — "agent" or "user" — and Archived
	// whether the owner put it away.
	TitleBy  string `json:"title_by,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	// Questions counts what this thread is waiting for the owner to
	// answer, so a sidebar can mark the threads that need a person.
	Questions int `json:"questions,omitempty"`
}

// ConversationPatch is what the owner may change about a conversation: its
// name (empty gives it back to the agent) and whether it is put away.
type ConversationPatch struct {
	Title    *string `json:"title,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

type Reply struct {
	// ID names the line for good: a quote of it, a comment on it, a
	// process fetched for it later all point here rather than at a time.
	ID         string `json:"id,omitempty"`
	ExchangeID string `json:"exchange_id,omitempty"`
	// AttemptID is assigned from the server's execution result, never from a
	// submitted command. Durable delivery proofs use it independently of UI history.
	AttemptID    string    `json:"attempt_id,omitempty"`
	At           time.Time `json:"at"`
	Conversation string    `json:"conversation"`
	ProjectID    string    `json:"project_id,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	Input        string    `json:"input,omitempty"`
	Title        string    `json:"title,omitempty"`
	Text         string    `json:"text"`
	Format       string    `json:"format,omitempty"` // markdown (default) | text
	Error        string    `json:"error,omitempty"`
	Kind         string    `json:"kind"` // reply | milestone | notice
	// Silent keeps a line out of the transcript without keeping it out of
	// the ledger: stopping a turn is an act on the turn, not a thing said
	// in the conversation, so "/cancel" and its receipt are recorded and
	// auditable but never drawn as messages.
	Silent bool `json:"silent,omitempty"`
	// Relayed marks a sent line Steve put into the conversation on the
	// owner's behalf rather than one the owner typed: a schedule firing,
	// a delegated task reporting back. It reads like a message but it is
	// a record of something that happened, so it cannot be rewritten.
	Relayed bool `json:"relayed,omitempty"`
	// Delivery distinguishes a retained channel result from confirmed delivery.
	// Values are confirmed, unconfirmed, suppressed, or unavailable.
	Delivery string `json:"delivery,omitempty"`
	// Process is how the reply was made, for the page to unfold; Injected
	// what the agent was given for the turn.
	Process  *Process  `json:"process,omitempty"`
	Injected *Injected `json:"injected,omitempty"`
	// Changes is what the turn changed, when an attempt captured it.
	Changes   *ChangeSummary    `json:"changes,omitempty"`
	Refs      []material.Ref    `json:"refs,omitempty"`
	Materials []material.Frozen `json:"materials,omitempty"`
}

// Setup is what an agent has in hand for a conversation: the assembled
// instructions a session opens with, what they are made of, and the MCP
// servers joined to it. It answers "what is this agent working with",
// which the turn-by-turn trace does not.
type Setup struct {
	Agent        string    `json:"agent"`
	Node         string    `json:"node,omitempty"`
	Harness      string    `json:"harness"`
	Model        string    `json:"model,omitempty"`
	Mode         string    `json:"mode,omitempty"`
	Instructions string    `json:"instructions,omitempty"`
	Sections     []Section `json:"sections,omitempty"`
	MCPServers   []string  `json:"mcp_servers,omitempty"`
	Applied      bool      `json:"applied"`
}

// Section is one piece of the assembled instructions: identity,
// language rule, the agent's own prompt, a skill, an extra or
// remembered text.
type Section struct {
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes"`
}

// Injected is what a turn gave the agent, as the console keeps it.
type Injected struct {
	Project           string            `json:"project,omitempty"`
	Workspace         string            `json:"workspace,omitempty"`
	Agent             string            `json:"agent"`
	Node              string            `json:"node,omitempty"`
	Harness           string            `json:"harness"`
	Model             string            `json:"model,omitempty"`
	Options           map[string]string `json:"options,omitempty"`
	Session           string            `json:"session,omitempty"`
	NewSession        bool              `json:"new_session"`
	InstructionsSent  bool              `json:"instructions_sent"`
	Instructions      string            `json:"instructions,omitempty"`
	InstructionsBytes int               `json:"instructions_bytes"`
	MCPServers        []string          `json:"mcp_servers,omitempty"`
	Fingerprint       string            `json:"fingerprint,omitempty"`
	Prompt            string            `json:"prompt,omitempty"`
	// Sections is what the instructions were made of, when they were
	// sent: the same breakdown the setup view shows.
	Sections []Section `json:"sections,omitempty"`
}

// Exchange names one submission throughout its queue, sent line and answer.
// Reply IDs still name individual transcript lines, including quoted lines.
type Exchange struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Input        string `json:"input"`
	// Prompt is what the agent is given when it differs from Input: a
	// continuation after a restart shows the notice and says "go on".
	Prompt string `json:"prompt,omitempty"`
	// History is what the thread said before this line, carried as text
	// because the agent's own session no longer holds it: a thread rewound
	// to an edited message answers from a session that never saw the turns
	// that were removed.
	History         string               `json:"history,omitempty"`
	Origin          string               `json:"origin,omitempty"`
	Requester       string               `json:"requester,omitempty"`
	ExpectedProject string               `json:"expected_project,omitempty"`
	ExpectedTask    string               `json:"expected_task,omitempty"`
	ResumeAdmission task.ResumeAdmission `json:"resume_admission,omitzero"`
	// Key is the durable submission identity within this conversation.
	// Client command IDs and platform deliveries have separate namespaces.
	Key        string            `json:"key,omitempty"`
	Quotes     []QuoteRef        `json:"quotes,omitempty"`
	Refs       []material.Ref    `json:"refs,omitempty"`
	Materials  []material.Frozen `json:"materials,omitempty"`
	Locale     string            `json:"locale,omitempty"`
	State      ExchangeState     `json:"state"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
	StartedAt  time.Time         `json:"started_at,omitzero"`
	ReplyID    string            `json:"reply_id,omitempty"`
}

var (
	ErrExchangeNotFound  = errors.New("exchange not found")
	ErrExchangeNotQueued = errors.New("exchange is no longer queued")
	ErrCommandConflict   = errors.New("command_id was already used with a different submission")
	// ErrRewindTargetGone is a line a submission meant to replace that is
	// no longer in the transcript: it was already rewound past, or the
	// thread was trimmed beyond it.
	ErrRewindTargetGone = errors.New("that message is no longer in this thread")
)

type TaskMetaPatch struct {
	Title    *string   `json:"title,omitempty"`
	Priority *string   `json:"priority,omitempty"`
	Labels   *[]string `json:"labels,omitempty"`
	Archived *bool     `json:"archived,omitempty"`
}

var ErrBusy = errors.New("busy")

package readmodel

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/view"
	"github.com/gopact-ai/steve/internal/nodewire"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServerConfig is where the read model is served and who may read it.
type ServerConfig struct {
	// Addr defaults to a loopback port. Binding anywhere else requires a
	// token: the snapshot names hosts, goals and agents, and that is not
	// something to hand to the network by accident.
	Addr  string
	Token string
}

// Server exposes the snapshot, the change stream and the dashboard.
type Server struct {
	console  Console
	admin    Admin
	model    *Model
	token    string
	listener net.Listener
}

// NewServer binds immediately so the caller knows the URL before serving.
func NewServer(model *Model, cfg ServerConfig) (*Server, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !loopback(addr) && strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("read model on %s needs a token: it reports hosts, goals and agents", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &Server{model: model, token: cfg.Token, listener: listener}, nil
}

func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }

func (s *Server) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", s.guard(s.state))
	mux.HandleFunc("GET /events", s.guard(s.events))
	mux.HandleFunc("POST /console/send", s.guard(s.consoleSend))
	mux.HandleFunc("GET /console/replies", s.guard(s.consoleReplies))
	mux.HandleFunc("GET /console/conversations", s.guard(s.consoleConversations))
	mux.HandleFunc("PUT /console/conversations/{id}", s.guard(s.consoleUpdateConversation))
	mux.HandleFunc("POST /console/nodes", s.guard(s.consoleAddNode))
	mux.HandleFunc("DELETE /console/nodes/{name}", s.guard(s.consoleRemoveNode))
	mux.HandleFunc("POST /console/agents", s.guard(s.consoleAddAgent))
	mux.HandleFunc("PUT /console/agents/{id}", s.guard(s.consoleUpdateAgent))
	mux.HandleFunc("DELETE /console/agents/{id}", s.guard(s.consoleRemoveAgent))
	mux.HandleFunc("POST /console/projects", s.guard(s.consoleAddProject))
	mux.HandleFunc("DELETE /console/projects/{id}", s.guard(s.consoleRemoveProject))
	mux.HandleFunc("GET /console/skills", s.guard(s.consoleSkills))
	mux.HandleFunc("GET /console/skills/{name}", s.guard(s.consoleSkill))
	mux.HandleFunc("PUT /console/skills/{name}", s.guard(s.consoleSetSkill))
	mux.HandleFunc("POST /console/skills/paths", s.guard(s.consoleAddSkillPath))
	mux.HandleFunc("DELETE /console/skills/paths", s.guard(s.consoleRemoveSkillPath))
	mux.HandleFunc("GET /console/skills/machines", s.guard(s.consoleMachineSkills))
	mux.HandleFunc("POST /console/skills/machines/refresh", s.guard(s.consoleRefreshMachineSkills))
	mux.HandleFunc("POST /console/skills/import", s.guard(s.consoleImportSkill))
	mux.HandleFunc("POST /console/skills/sources", s.guard(s.consoleAddSkillSource))
	mux.HandleFunc("POST /console/skills/sources/update", s.guard(s.consoleUpdateSkillSources))
	mux.HandleFunc("DELETE /console/skills/sources/{slug}", s.guard(s.consoleRemoveSkillSource))
	mux.HandleFunc("GET /console/mcp", s.guard(s.consoleMCP))
	mux.HandleFunc("POST /console/mcp/probe", s.guard(s.consoleProbeMCP))
	mux.HandleFunc("POST /console/mcp/adopt", s.guard(s.consoleAdoptMCP))
	mux.HandleFunc("DELETE /console/mcp", s.guard(s.consoleRemoveMCP))
	mux.HandleFunc("GET /console/mcp/registry", s.guard(s.consoleMCPRegistry))
	mux.HandleFunc("POST /console/mcp/install", s.guard(s.consoleInstallMCP))
	mux.HandleFunc("GET /console/home", s.guard(s.consoleHome))
	mux.HandleFunc("PUT /console/home/{name}", s.guard(s.consoleSetHomeFile))
	mux.HandleFunc("PUT /console/memory/{project}", s.guard(s.consoleSetProjectMemory))
	mux.HandleFunc("GET /console/selectors", s.guard(s.consoleSelectors))
	mux.HandleFunc("PUT /console/preferences", s.guard(s.consoleSetPreferences))
	mux.HandleFunc("GET /console/tasks/{task}", s.guard(s.consoleTask))
	mux.HandleFunc("PATCH /console/tasks/{task}/meta", s.guard(s.consoleTaskMeta))
	mux.HandleFunc("GET /console/tasks/{task}/attempts", s.guard(s.consoleTaskAttempts))
	mux.HandleFunc("GET /console/attempts/{attempt}/tree", s.guard(s.consoleAttemptTree))
	mux.HandleFunc("GET /console/attempts/{attempt}/file", s.guard(s.consoleAttemptFile))
	mux.HandleFunc("GET /console/attempts/{attempt}/changes", s.guard(s.consoleAttemptChanges))
	mux.HandleFunc("GET /console/attempts/{attempt}/diff", s.guard(s.consoleAttemptDiff))
	mux.HandleFunc("POST /console/projects/{id}/workspaces", s.guard(s.consoleAddWorkspace))
	mux.HandleFunc("DELETE /console/projects/{id}/workspaces/{node}", s.guard(s.consoleRemoveWorkspace))
	mux.HandleFunc("GET /console/nodes/{name}/settings", s.guard(s.nodeSettings))
	mux.HandleFunc("PUT /console/nodes/{name}/settings", s.guard(s.nodeSettings))
	mux.HandleFunc("GET /bootstrap/{name}", s.bootstrap)
	mux.HandleFunc("GET /dist/steve-node", s.nodeBinary)
	mux.HandleFunc("GET /console/context", s.guard(s.consoleContext))
	mux.HandleFunc("GET /console/verbs", s.guard(s.consoleVerbs))
	mux.HandleFunc("GET /console/suggest", s.guard(s.consoleSuggest))
	mux.HandleFunc("GET /history", s.guard(s.history))
	// The bundle is hashed, static code with nothing of the fleet in it,
	// and the browser fetches it without the token the shell was opened
	// with; it is served open. Everything that carries data stays guarded.
	mux.HandleFunc("GET /assets/", s.page)
	mux.HandleFunc("GET /", s.guard(s.page))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Close() error { return s.listener.Close() }

// guard checks the token when one is configured. Loopback-only deployments
// leave it empty and rely on the bind address, which is the same posture the
// messaging server already takes.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if presented == "" {
		presented = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	snap := s.model.Snapshot(r.Context())
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snap); err != nil {
		log.Printf("readmodel: encode snapshot: %v", err)
	}
}

// events streams changes as server-sent events, so a renderer learns about a
// step transition when it happens instead of on its next poll.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	stream, stop := s.model.Subscribe(r.Context())
	defer stop()

	// Replay what just happened so a renderer attaching mid-flight is not
	// staring at nothing until the next change.
	for _, ev := range s.model.Recent() {
		writeEvent(w, ev)
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-stream:
			if !open {
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // a bare ":port" listens on every interface
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

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

type Console interface {
	Send(ctx context.Context, conversation, input string) (Reply, error)
	// SendCommand is Send with an idempotency key from the page.
	SendCommand(ctx context.Context, conversation, input, commandID string) (Reply, error)
	// SendCommandWith carries quotes of other lines along with the input.
	SendCommandWith(ctx context.Context, conversation, input, commandID string, quotes []QuoteRef) (Reply, error)
	Replies(conversation string) []Reply
	// Conversations names every console conversation with a transcript;
	// Summaries describes each one for a sidebar.
	Conversations() []string
	Summaries(ctx context.Context) []Conversation
	Update(ctx context.Context, conversation string, patch ConversationPatch) error
	// Context is where a conversation stands; Verbs is what it can be told.
	Context(ctx context.Context, conversation string) (Context, error)
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

// AddProjectRequest is the page declaring a project: a name, the machine
// and directory it lives in, how agents may change it, its data level.
type AddProjectRequest struct {
	ID    string `json:"id"`
	Node  string `json:"node,omitempty"`
	Path  string `json:"path"`
	Repo  string `json:"repo,omitempty"`
	Level string `json:"level,omitempty"`
}

// AddWorkspaceRequest is the page giving a project a copy on a machine:
// "adopt" a directory already there, or "clone" the project into one.
type AddWorkspaceRequest struct {
	Node   string `json:"node,omitempty"`
	Path   string `json:"path"`
	Origin string `json:"origin,omitempty"`
}

// AddAgentRequest is the page adding an agent: an id, the AI tool it
// runs, the machine it runs on ("" is the hub), a preferred model.
type AddAgentRequest struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Node    string `json:"node,omitempty"`
	Model   string `json:"model,omitempty"`
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
	// AddProject declares a project and records it in the config file;
	// RemoveProject retires one and drops it from the file.
	AddProject(ctx context.Context, req AddProjectRequest) error
	RemoveProject(ctx context.Context, id string) error
	// AddWorkspace gives a project a copy on a machine — a directory that
	// is there, or one cloned into place; RemoveWorkspace forgets one.
	AddWorkspace(ctx context.Context, projectID string, req AddWorkspaceRequest) error
	RemoveWorkspace(ctx context.Context, projectID, node string) error
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
	// TaskAttempts are a task's attempts from the ledger, newest first.
	TaskAttempts(ctx context.Context, task string) ([]AttemptView, error)
	// Selectors reads what an agent offers in a thread (opening a session
	// when none is live); SetPreferences records choices and rolls the
	// session over so the next turn honours them.
	Selectors(ctx context.Context, conversation, agent string) (Selectors, error)
	SetPreferences(ctx context.Context, conversation, agent string, patch map[string]string) error
	SetTaskMeta(ctx context.Context, task string, patch TaskMetaPatch) (Task, error)
	// AttemptTree lists a directory of an attempt's snapshot; AttemptFile
	// reads one file of it. Both bounded, both owner-only.
	AttemptTree(ctx context.Context, attempt, dir string) (TreeView, error)
	AttemptFile(ctx context.Context, attempt, path string) (FileView, error)
}

// SkillsView is the skills page: where skills are looked for, every
// skill found there with whether it is handed to agents, and whether
// each machine holds the current bundle.
type SkillsView struct {
	Fingerprint string   `json:"fingerprint"`
	SearchPaths []string `json:"search_paths"`
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
	Deployments []MCPDeployment `json:"deployments"`
	Platform    []MCPPlatform   `json:"platform"`
	Machines    []MCPMachine    `json:"machines"`
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
	Attempt   string           `json:"attempt"`
	Commit    string           `json:"commit"`
	Which     string           `json:"which"` // result | base
	Dir       string           `json:"dir"`
	Entries   []artifact.Entry `json:"entries"`
	Truncated bool             `json:"truncated,omitempty"`
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

// TaskDetail is one task joined for the page: the task, its plan, its
// children and its attempts from the ledger.
type TaskDetail struct {
	Task     Task          `json:"task"`
	Plan     *Plan         `json:"plan,omitempty"`
	Children []Task        `json:"children"`
	Attempts []AttemptView `json:"attempts"`
}

// ChangeSummary is what a turn changed, as the reply keeps it: the
// attempt to ask for the index, and the count. Paths and diffs are
// read on demand, never stored with the reply.
type ChangeSummary struct {
	Attempt  string `json:"attempt"`
	Project  string `json:"project,omitempty"`
	Base     string `json:"base,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	Files    int    `json:"files"`
	Note     string `json:"note,omitempty"`
}

// ChangeIndex is the files an attempt changed, bounded.
type ChangeIndex struct {
	Attempt   string            `json:"attempt"`
	Project   string            `json:"project,omitempty"`
	Base      string            `json:"base,omitempty"`
	Artifact  string            `json:"artifact,omitempty"`
	Changes   []artifact.Change `json:"changes"`
	Truncated bool              `json:"truncated,omitempty"`
	Note      string            `json:"note,omitempty"`
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
	ID      string `json:"id"`
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
	ID           string    `json:"id,omitempty"`
	At           time.Time `json:"at"`
	Conversation string    `json:"conversation"`
	Input        string    `json:"input,omitempty"`
	Title        string    `json:"title,omitempty"`
	Text         string    `json:"text"`
	Error        string    `json:"error,omitempty"`
	Kind         string    `json:"kind"` // reply | milestone | notice
	// Process is how the reply was made, for the page to unfold; Injected
	// what the agent was given for the turn.
	Process  *Process  `json:"process,omitempty"`
	Injected *Injected `json:"injected,omitempty"`
	// Changes is what the turn changed, when an attempt captured it.
	Changes *ChangeSummary `json:"changes,omitempty"`
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
}

// SetConsole wires the acting half of the page.
func (s *Server) SetConsole(c Console) { s.console = c }

// SetAdmin wires adding machines and agents from the page.
func (s *Server) SetAdmin(a Admin) { s.admin = a }

func (s *Server) consoleAddNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding machines is not wired", http.StatusNotImplemented)
		return
	}
	var req AddNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	req.HubURL = scheme + "://" + r.Host
	out, err := s.admin.AddNode(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) consoleRemoveNode(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveNode(r.Context(), r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleAddProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var req AddProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddProject(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var spec AgentSpec
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&spec); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.UpdateAgent(r.Context(), r.PathValue("id"), spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveAgent(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleRemoveProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveProject(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleAddWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var req AddWorkspaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddWorkspace(r.Context(), r.PathValue("id"), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleRemoveWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveWorkspace(r.Context(), r.PathValue("id"), r.PathValue("node")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ErrBusy is an admin action refused because something is running where
// it would act.
var ErrBusy = errors.New("busy")

func (s *Server) adminOr(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) consoleSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.Skills(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Server) consoleSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	doc, err := s.admin.SkillContent(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (s *Server) consoleSetSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetSkill(r.Context(), r.PathValue("name"), req.Enabled); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleAddSkillPath(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddSkillPath(r.Context(), req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleRemoveSkillPath(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveSkillPath(r.Context(), r.URL.Query().Get("path")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleMachineSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out := s.admin.MachineSkills(r.Context())
	if out == nil {
		out = []MachineSkills{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"machines": out})
}

func (s *Server) consoleRefreshMachineSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out := s.admin.RefreshMachineSkills(r.Context())
	if out == nil {
		out = []MachineSkills{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"machines": out})
}

func (s *Server) consoleImportSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Node string `json:"node"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, err := s.admin.ImportSkill(r.Context(), req.Node, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name})
}

func (s *Server) consoleAddSkillSource(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Spec string `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	src, err := s.admin.AddSkillSource(r.Context(), req.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(src)
}

func (s *Server) consoleUpdateSkillSources(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out, err := s.admin.UpdateSkillSources(r.Context())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if out == nil {
		out = []SkillSource{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"sources": out})
}

func (s *Server) consoleRemoveSkillSource(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveSkillSource(r.Context(), r.PathValue("slug")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.MCP(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Server) consoleProbeMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct{ Node, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	probe, err := s.admin.ProbeMCP(r.Context(), req.Node, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": probe.OK, "probe": probe})
}

func (s *Server) consoleAdoptMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct{ Node, Source, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AdoptMCP(r.Context(), req.Node, req.Source, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleRemoveMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveMCP(r.Context(), r.URL.Query().Get("node"), r.URL.Query().Get("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleMCPRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	entries, err := s.admin.SearchMCPRegistry(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if entries == nil {
		entries = []MCPRegistryEntry{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}

func (s *Server) consoleInstallMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req InstallMCPRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.InstallMCP(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleHome(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.Home(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Server) consoleSetHomeFile(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetHomeFile(r.Context(), r.PathValue("name"), req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleSelectors(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	sel, err := s.admin.Selectors(r.Context(), r.URL.Query().Get("conversation"), r.URL.Query().Get("agent"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sel.Models == nil {
		sel.Models = []view.Choice{}
	}
	if sel.Options == nil {
		sel.Options = []view.Option{}
	}
	_ = json.NewEncoder(w).Encode(sel)
}

func (s *Server) consoleSetPreferences(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Conversation string            `json:"conversation"`
		Agent        string            `json:"agent"`
		Patch        map[string]string `json:"patch"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetPreferences(r.Context(), req.Conversation, req.Agent, req.Patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": "下一轮以新会话开始"})
}

// consoleTask joins one task for the page: the read model's task, plan
// and children, and the ledger's attempts through the admin.
func (s *Server) consoleTask(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("task")
	snap := s.model.Snapshot(r.Context())
	var found *Task
	for i := range snap.Tasks {
		if snap.Tasks[i].ID == id {
			found = &snap.Tasks[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "no task "+id, http.StatusNotFound)
		return
	}
	detail := TaskDetail{Task: *found, Children: []Task{}, Attempts: []AttemptView{}}
	for _, t := range snap.Tasks {
		if t.Parent == id {
			detail.Children = append(detail.Children, t)
		}
	}
	for i := range snap.Plans {
		if snap.Plans[i].TaskID == id {
			p := snap.Plans[i]
			detail.Plan = &p
			break
		}
	}
	if attempts, err := s.admin.TaskAttempts(r.Context(), id); err == nil && attempts != nil {
		detail.Attempts = attempts
	}
	_ = json.NewEncoder(w).Encode(detail)
}

func (s *Server) consoleTaskAttempts(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	attempts, err := s.admin.TaskAttempts(r.Context(), r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if attempts == nil {
		attempts = []AttemptView{}
	}
	_ = json.NewEncoder(w).Encode(attempts)
}

func (s *Server) consoleAttemptTree(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	tree, err := s.admin.AttemptTree(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if tree.Entries == nil {
		tree.Entries = []artifact.Entry{}
	}
	_ = json.NewEncoder(w).Encode(tree)
}

func (s *Server) consoleAttemptFile(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	file, err := s.admin.AttemptFile(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(file)
}

func (s *Server) consoleAttemptChanges(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	index, err := s.admin.AttemptChanges(r.Context(), r.PathValue("attempt"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if index.Changes == nil {
		index.Changes = []artifact.Change{}
	}
	_ = json.NewEncoder(w).Encode(index)
}

func (s *Server) consoleAttemptDiff(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	diff, err := s.admin.AttemptDiff(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(diff)
}

func (s *Server) consoleSetProjectMemory(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetProjectMemory(r.Context(), r.PathValue("project"), req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) nodeSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	name := r.PathValue("name")
	var out nodewire.Settings
	var err error
	if r.Method == http.MethodPut {
		var set nodewire.Settings
		if derr := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); derr != nil {
			http.Error(w, "bad request: "+derr.Error(), http.StatusBadRequest)
			return
		}
		out, err = s.admin.SetNodeSettings(r.Context(), name, set)
	} else {
		out, err = s.admin.NodeSettings(r.Context(), name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"settings": out})
}

func (s *Server) consoleAddAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding agents is not wired", http.StatusNotImplemented)
		return
	}
	var req AddAgentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddAgent(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// bootstrap hands a machine its start script. The machine presents its
// own node token, not the owner's: the script is the machine's business.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	script, ok := s.admin.Bootstrap(r.PathValue("name"), r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "unknown machine or wrong token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, script)
}

func (s *Server) nodeBinary(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	path, ok := s.admin.NodeBinary(r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "no binary here, or wrong token", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func (s *Server) consoleSend(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		http.Error(w, "the console is not enabled on this gateway", http.StatusNotImplemented)
		return
	}
	var req struct {
		Conversation string     `json:"conversation"`
		Input        string     `json:"input"`
		CommandID    string     `json:"command_id"`
		Quotes       []QuoteRef `json:"quotes,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}
	if req.Conversation == "" {
		req.Conversation = "console:main"
	}
	var reply Reply
	var err error
	if len(req.Quotes) > 0 {
		reply, err = s.console.SendCommandWith(r.Context(), req.Conversation, req.Input, req.CommandID, req.Quotes)
	} else {
		reply, err = s.console.SendCommand(r.Context(), req.Conversation, req.Input, req.CommandID)
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil && strings.Contains(err.Error(), "already running") {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "reply": reply})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"reply": reply})
}

func (s *Server) consoleContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	ctx, err := s.console.Context(r.Context(), conversation)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if ctx.Agents == nil {
		ctx.Agents = []AgentChoice{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "context": ctx})
}

// history pages what happened, newest first; before is the ledger
// sequence to continue from, as the previous page's next.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, next, err := s.model.History(r.Context(), before, limit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []HistoryEntry{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries, "next": next})
}

func (s *Server) consoleSuggest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	items := []Suggestion{}
	if s.console != nil {
		conversation := r.URL.Query().Get("conversation")
		if conversation == "" {
			conversation = "console:main"
		}
		items = append(items, s.console.Suggest(r.Context(), conversation, r.URL.Query().Get("q"))...)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"suggestions": items})
}

func (s *Server) consoleVerbs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	verbs := []Verb{}
	if s.console != nil {
		verbs = append(verbs, s.console.Verbs()...)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"verbs": verbs})
}

func (s *Server) consoleConversations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "conversations": []Conversation{}})
		return
	}
	list := s.console.Summaries(r.Context())
	if list == nil {
		list = []Conversation{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "conversations": list})
}

func (s *Server) consoleUpdateConversation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		http.Error(w, "console is not enabled", http.StatusNotImplemented)
		return
	}
	var patch ConversationPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&patch); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.console.Update(r.Context(), r.PathValue("id"), patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) consoleReplies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "replies": []Reply{}, "conversations": []string{}})
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	names := s.console.Conversations()
	if names == nil {
		names = []string{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replies": s.console.Replies(conversation), "conversations": names})
}

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/view"
)

const (
	Codex      = "codex"
	ClaudeCode = "claude-code"
	Grok       = "grok"
	Kimi       = "kimi"

	EnvCodexHome       = "CODEX_HOME"
	EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"
	EnvGrokHome        = "GROK_HOME"
	EnvKimiCodeHome    = "KIMI_CODE_HOME"
)

type Config struct {
	PluginInstructions string
	Command            string
	Args               []string
	ProcessDir         string
	Env                []string
	Permission         string
}

// Placement is where one session's agent process runs. An empty Node means
// the hub's own machine; anything else is resolved through the node
// registry. Harness alone is not enough to key a process: the same harness
// id on two machines is two different agents, with different binaries,
// different credentials and different reachable models.
type Placement struct {
	Node    string
	Harness string
}

func (p Placement) key() string {
	if p.Node == "" {
		return p.Harness
	}
	return p.Node + "/" + p.Harness
}

func (p Placement) String() string { return p.key() }

// Transports resolves a placement to the way its process is started. The
// node registry implements it; a hub with no remote nodes needs none.
type Transports interface {
	Transport(node, harness string) acphost.Transport
}

type Manager struct {
	pluginRefs        map[*acphost.Host]plugins.RuntimeRef
	pluginRuntimes    PluginRuntimeProvider
	suspended         map[string]bool
	configs           map[string]Config
	remote            Transports
	observe           Observer
	nodeSessionBinder func(context.Context, Placement, string, string) (context.Context, error)
	stopRegistrar     func(context.Context, string, func(context.Context) error) error
	mu                sync.Mutex
	hosts             map[string]*acphost.Host
	stopped           bool
}

func NewManager(configs map[string]Config) (*Manager, error) {
	for id, cfg := range configs {
		if id == "" || cfg.Command == "" {
			return nil, fmt.Errorf("harness id and command are required")
		}
		if _, err := permission.New(cfg.Permission); err != nil {
			return nil, fmt.Errorf("harness %q: %w", id, err)
		}
	}
	return &Manager{configs: cloneConfigs(configs), hosts: map[string]*acphost.Host{}}, nil
}

func cloneConfig(cfg Config) Config {
	cfg.Args = slices.Clone(cfg.Args)
	cfg.Env = slices.Clone(cfg.Env)
	return cfg
}

func cloneConfigs(configs map[string]Config) map[string]Config {
	owned := make(map[string]Config, len(configs))
	for id, cfg := range configs {
		owned[id] = cloneConfig(cfg)
	}
	return owned
}

// Publish installs a previously validated configuration after persistence.
// Existing hosts keep their configuration and sessions until they restart.
// This performs no I/O and cannot partially fail.
func (m *Manager) Publish(prepared *Manager) {
	if m == prepared {
		return
	}
	prepared.mu.Lock()
	configs := cloneConfigs(prepared.configs)
	prepared.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs = configs
}

// Set adds or replaces one harness's configuration at runtime. Hosts
// already running the old configuration keep it until they restart.
func (m *Manager) Set(id string, cfg Config) error {
	if id == "" || cfg.Command == "" {
		return fmt.Errorf("harness id and command are required")
	}
	if _, err := permission.New(cfg.Permission); err != nil {
		return fmt.Errorf("harness %q: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[id] = cloneConfig(cfg)
	return nil
}

// Remove forgets a harness; a host running it is closed.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.configs, id)
	var closing []*acphost.Host
	for key, host := range m.hosts {
		if strings.HasSuffix(key, "/"+id) || key == id {
			closing = append(closing, host)
			delete(m.hosts, key)
		}
	}
	m.mu.Unlock()
	for _, host := range closing {
		host.Close()
	}
}

// SetTransports wires remote placements. Without it every placement must be
// local, and one naming a node is refused rather than silently run here —
// running a "GPU" step on the wrong machine is worse than not running it.
// Observer learns what a session reported when it opened: which model the
// harness runs here and which it offers. That is how the fleet's model
// column fills without anyone typing model names into a config.
type Observer func(at Placement, settings view.Settings)

// SetObserver installs the observer; nil turns it off.
func (m *Manager) SetObserver(observe Observer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observe = observe
}

func (m *Manager) SetTransports(remote Transports) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remote = remote
}

func (m *Manager) OpenSession(ctx context.Context, at Placement, upstreamID, workdir string, servers []acp.MCPServer) (Runner, error) {
	var bindErr error
	ctx, bindErr = m.bindNodeSession(ctx, at, upstreamID, workdir)
	if bindErr != nil {
		return nil, bindErr
	}
	if runner, handled, err := m.openNodeSession(ctx, at, upstreamID, workdir, servers); handled {
		return runner, err
	}
	if PluginProfile(ctx) != nil {
		return m.openPluginSession(ctx, at, upstreamID, workdir, servers)
	}
	if strings.HasPrefix(upstreamID, "ps_") {
		return nil, errors.New("plugin runtime reference is required to resume this session")
	}
	if workdir == "" {
		return nil, fmt.Errorf("agent workspace is required")
	}
	// A remote workspace lives on the node's filesystem; only the node can
	// create it, and guessing a path here would make a directory on the
	// wrong machine.
	if at.Node == "" {
		if err := os.MkdirAll(workdir, 0o700); err != nil {
			return nil, fmt.Errorf("create agent workspace: %w", err)
		}
	}
	host, err := m.host(at)
	if err != nil {
		return nil, err
	}
	id, generation, err := host.OpenSession(ctx, acp.SessionID(upstreamID), acphost.SessionConfig{Workdir: workdir, MCPServers: servers})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	stopped, observe := m.stopped, m.observe
	m.mu.Unlock()
	if stopped {
		host.Close()
		return nil, fmt.Errorf("harness manager is stopped")
	}
	session := &Session{at: at, id: id, generation: generation, host: host, observe: observe}
	if observe != nil {
		observe(at, session.Settings())
	}
	return session, nil
}

// SupportsHTTPMCP reports whether the harness's agent can take an HTTP MCP
// server in its session config.
func (m *Manager) SupportsHTTPMCP(ctx context.Context, at Placement) (bool, error) {
	var bindErr error
	ctx, bindErr = m.bindNodeSession(ctx, at, "", "")
	if bindErr != nil {
		return false, bindErr
	}
	if binding, bound := NodeSessionFromContext(ctx); bound {
		m.mu.Lock()
		transport, ok := m.remote.(NodeSessionTransport)
		m.mu.Unlock()
		if !ok {
			return false, ErrNodeSessionUnavailable
		}
		node := at.Node
		if node == "" {
			node = binding.Binding.NodeID
		}
		state, err := transport.NodeSession(ctx, node, nodewire.SessionRequest{Action: nodewire.SessionActionCapabilities, Authority: binding.Authority, Binding: binding.Binding, Harness: at.Harness})
		return state.SupportsHTTPMCP, err
	}
	host, err := m.host(at)
	if err != nil {
		return false, err
	}
	return host.SupportsHTTPMCP(ctx)
}

func (m *Manager) CloseSession(ctx context.Context, at Placement, upstreamID string) error {
	if handled, err := m.closePluginSession(ctx, at, upstreamID); handled {
		return err
	}
	var bindErr error
	ctx, bindErr = m.bindNodeSession(ctx, at, upstreamID, "")
	if bindErr != nil {
		return bindErr
	}
	if strings.HasPrefix(upstreamID, "ns_") {
		binding, ok := NodeSessionFromContext(ctx)
		if !ok {
			return ErrNodeSessionUnavailable
		}
		m.mu.Lock()
		transport, ok := m.remote.(NodeSessionTransport)
		m.mu.Unlock()
		if !ok {
			return ErrNodeSessionUnavailable
		}
		node := at.Node
		if node == "" {
			node = binding.Binding.NodeID
		}
		_, err := transport.NodeSession(ctx, node, nodewire.SessionRequest{Action: nodewire.SessionActionClose, ID: upstreamID, Authority: binding.Authority, Binding: binding.Binding})
		return err
	}
	m.mu.Lock()
	host := m.hosts[at.key()]
	m.mu.Unlock()
	if host == nil || upstreamID == "" {
		return nil
	}
	return host.CloseSession(ctx, acp.SessionID(upstreamID))
}

func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	hosts := make([]*acphost.Host, 0, len(m.hosts))
	for _, host := range m.hosts {
		hosts = append(hosts, host)
	}
	refs := make(map[*acphost.Host]plugins.RuntimeRef, len(m.pluginRefs))
	for host, ref := range m.pluginRefs {
		refs[host] = ref
	}
	m.mu.Unlock()
	for _, host := range hosts {
		host.Close()
		if ref, ok := refs[host]; ok && host.AllProcessesStopped() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := m.pluginUsage(ctx, Placement{Node: ref.Selection.Node, Harness: ref.Selection.Harness}, ref, false)
			cancel()
			if err != nil {
				slog.Error("harness: plugin stop receipt failed", "error", err)
			}
		}
	}
}

// Restart closes every live harness process so the next session starts with
// a freshly scanned skill directory. In-flight turns must be idle first.
func (m *Manager) Restart() error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return fmt.Errorf("harness manager is stopped")
	}
	hosts := make([]*acphost.Host, 0, len(m.hosts))
	for key, host := range m.hosts {
		if strings.HasPrefix(key, "plugin/") {
			continue
		}
		hosts = append(hosts, host)
		delete(m.hosts, key)
	}
	m.mu.Unlock()
	for _, host := range hosts {
		host.Close()
	}
	return nil
}

func (m *Manager) host(at Placement) (*acphost.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.suspended["*"] || m.suspended[at.Node] {
		return nil, fmt.Errorf("service is restarting")
	}
	if m.stopped {
		return nil, fmt.Errorf("harness manager is stopped")
	}
	key := at.key()
	if host := m.hosts[key]; host != nil {
		return host, nil
	}
	cfg, ok := m.configs[at.Harness]
	if !ok && at.Node == "" {
		return nil, fmt.Errorf("unknown harness %q", at.Harness)
	}
	if at.Node != "" && cfg.Permission == "" {
		// A remote node owns its own tool declaration. A matching local
		// policy remains explicit configuration; otherwise new remote tools
		// use the same read policy as a first local registration.
		cfg.Permission = permission.PolicyRead
	}
	broker, err := permission.New(cfg.Permission)
	if err != nil {
		return nil, err
	}
	hostCfg := acphost.Config{
		Command: cfg.Command, Args: cfg.Args, ProcessDir: cfg.ProcessDir, Env: cfg.Env, Permission: broker,
	}
	if at.Node != "" {
		if m.remote == nil {
			return nil, fmt.Errorf("agent is placed on node %q but no nodes are configured", at.Node)
		}
		// The command line is the node's own fact; the hub only says which
		// harness it wants, and the node starts it from its own config.
		hostCfg = acphost.Config{Transport: m.remote.Transport(at.Node, at.Harness), Permission: broker}
	}
	host := acphost.New(hostCfg)
	m.hosts[key] = host
	return host, nil
}

// ErrTurnCanceled is re-exported from acphost so coordinators can recognize
// a turn the agent ended itself without importing the host package.
var ErrTurnCanceled = acphost.ErrTurnCanceled

// ErrStopUnconfirmed requires writer exclusion until process termination is verified.
var ErrStopUnconfirmed = acphost.ErrStopUnconfirmed

type Media struct {
	MIME string
	Data []byte
	URI  string // nonempty for an embedded resource; empty for an image
}

type Runner interface {
	ID() string
	Prompt(context.Context, string, func(view.Progress)) (string, []string, error)
	Cancel(context.Context) error
	Abort()
}

// Configurable is a Runner whose agent exposes selectors Steve can change.
// It is optional: an agent that reports no model selector simply does not
// satisfy it.
type Configurable interface {
	Runner
	Settings() view.Settings
	ModelChoices() (string, []view.Choice)
	SetModel(context.Context, string, string) error
	// SetOption sets any selector by id; SetModel is the model's case.
	SetOption(context.Context, string, string) error
}

// Reobserver is a Runner that can report its settings to its observer
// again after a preference changed them.
type Reobserver interface {
	Reobserve()
}

var (
	_ Reobserver = (*Session)(nil)
	_ Reobserver = (*managedSession)(nil)
)

// ApplyPreferences sets what an agent's configuration pins on a session
// that has just opened: the model, and any other selector the harness
// exposes (reasoning effort, thinking, mode) by option id. The agent is
// the authority on what it offers: a preference it cannot honour is
// logged and skipped, never a failed session.
func ApplyPreferences(ctx context.Context, r Runner, agentID, model string, options map[string]string) {
	if model == "" && len(options) == 0 {
		return
	}
	configurable, ok := r.(Configurable)
	if !ok {
		return
	}
	changed := false
	defer func() {
		if changed {
			if r, ok := r.(Reobserver); ok {
				r.Reobserve()
			}
		}
	}()
	if model != "" {
		optionID, choices := configurable.ModelChoices()
		if optionID == "" || len(choices) == 0 {
			slog.Warn(fmt.Sprintf("harness: agent %q prefers model %q but exposes no model selector", agentID, model), "agent", agentID)
		} else if picked, ok := MatchChoice(choices, model); !ok {
			slog.Warn(fmt.Sprintf("harness: agent %q prefers model %q, not among %d offered", agentID, model, len(choices)), "agent", agentID)
		} else if err := configurable.SetModel(ctx, optionID, picked.Value); err != nil {
			slog.Error(fmt.Sprintf("harness: agent %q set preferred model %q: %v", agentID, model, err), "agent", agentID)
		} else {
			changed = true
		}
	}
	if len(options) == 0 {
		return
	}
	exposed := configurable.Settings().Options
	for id, want := range options {
		var found *view.Option
		for i := range exposed {
			if exposed[i].ID == id {
				found = &exposed[i]
				break
			}
		}
		if found == nil {
			slog.Warn(fmt.Sprintf("harness: agent %q pins option %q but the harness exposes no such selector", agentID, id), "agent", agentID)
			continue
		}
		if found.Category == "model" {
			continue // the model is handled above, by name
		}
		picked, ok := MatchChoice(found.Choices, want)
		if !ok {
			slog.Warn(fmt.Sprintf("harness: agent %q pins %s=%q, not among %d choices", agentID, id, want, len(found.Choices)), "agent", agentID)
			continue
		}
		if picked.Value == found.Current {
			continue
		}
		if err := configurable.SetOption(ctx, id, picked.Value); err != nil {
			slog.Error(fmt.Sprintf("harness: agent %q set %s=%q: %v", agentID, id, want, err), "agent", agentID)
		} else {
			slog.Info(fmt.Sprintf("harness: agent %q set %s=%q (was %q)", agentID, id, picked.Value, found.Current), "agent", agentID)
			changed = true
		}
	}
}

// MatchChoice finds the choice a person meant: by value, by label, or by
// a unique case-insensitive prefix of either.
func MatchChoice(choices []view.Choice, want string) (view.Choice, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return view.Choice{}, false
	}
	lower := strings.ToLower(want)
	for _, c := range choices {
		if c.Value == want || c.Label == want {
			return c, true
		}
	}
	for _, c := range choices {
		if strings.EqualFold(c.Value, want) || strings.EqualFold(c.Label, want) {
			return c, true
		}
	}
	var hits []view.Choice
	for _, c := range choices {
		if strings.HasPrefix(strings.ToLower(c.Value), lower) || strings.HasPrefix(strings.ToLower(c.Label), lower) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return view.Choice{}, false
}

type TurnRunner interface {
	Runner
	PromptTurn(context.Context, string, []Media, permission.AskFunc, acphost.AskUserFunc, func(view.Progress)) (string, []string, error)
}

type Session struct {
	plugin             *plugins.RuntimeRef
	pluginInstructions string
	at                 Placement
	id                 acp.SessionID
	generation         uint64
	host               *acphost.Host
	// observe is the manager's observer, kept so a session whose
	// selectors were just pinned can report its settings again.
	observe Observer
}

// Stopped reports verified termination of this session's original process.
func (s *Session) Stopped() bool { return s.host.ProcessStopped(s.generation) }

// Reobserve reports the session's settings to the observer once more:
// what the book keeps should be what the session runs with after its
// agent's pins were applied, not what it opened with.
func (s *Session) Reobserve() {
	if s.observe != nil {
		s.observe(s.at, s.Settings())
	}
}

func (s *Session) ID() string {
	if s.plugin != nil {
		return profileSessionID(*s.plugin, s.id)
	}
	return string(s.id)
}

func (s *Session) Prompt(ctx context.Context, text string, progress func(view.Progress)) (string, []string, error) {
	return s.PromptTurn(ctx, text, nil, nil, nil, progress)
}

func (s *Session) PromptTurn(
	ctx context.Context,
	text string,
	media []Media,
	ask permission.AskFunc,
	askUser acphost.AskUserFunc,
	progress func(view.Progress),
) (string, []string, error) {
	if s.pluginInstructions != "" {
		text = s.pluginInstructions + "\n\n" + text
	}
	images := make([]acphost.Image, 0, len(media))
	for _, item := range media {
		if len(item.Data) == 0 {
			continue
		}
		images = append(images, acphost.Image{MIME: item.MIME, Data: item.Data, URI: item.URI})
	}
	return s.host.PromptTurn(ctx, s.id, s.generation, text, images, ask, askUser, s.stamp(progress))
}

// stamp names the harness and the machine on every snapshot. The host
// reports the model and mode because only the agent knows them; which
// harness is speaking, and where, are Steve's own facts, so they are added
// here rather than plumbed down.
func (s *Session) stamp(progress func(view.Progress)) func(view.Progress) {
	if progress == nil {
		return nil
	}
	return func(p view.Progress) {
		p.Settings.Harness = s.at.Harness
		p.Settings.Node = s.at.Node
		progress(p)
	}
}

// Settings reports how the agent has this session configured.
func (s *Session) Settings() view.Settings {
	out := s.host.Settings(s.id)
	out.Harness = s.at.Harness
	out.Node = s.at.Node
	return out
}

// ModelChoices reports the config option id for the model selector and the
// models it offers. Both are empty when the agent exposes no such selector.
func (s *Session) ModelChoices() (string, []view.Choice) {
	id, choices := s.host.ModelChoices(s.id)
	return string(id), choices
}

func (s *Session) SetModel(ctx context.Context, optionID, value string) error {
	return s.host.SetOption(ctx, s.id, s.generation, acp.SessionConfigID(optionID), value)
}

// SetOption sets any selector the agent exposes, by id.
func (s *Session) SetOption(ctx context.Context, optionID, value string) error {
	return s.host.SetOption(ctx, s.id, s.generation, acp.SessionConfigID(optionID), value)
}

func (s *Session) Cancel(ctx context.Context) error { return s.host.Cancel(ctx, s.id, s.generation) }

func (s *Session) Abort() {
	// The ACP subprocess is the cancellation boundary; use per-session
	// processes if cross-session disruption matters.
	s.host.Abort(s.generation)
}

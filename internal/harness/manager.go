package harness

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
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
	Command    string
	Args       []string
	ProcessDir string
	Env        []string
	Permission string
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
	configs map[string]Config
	remote  Transports
	observe Observer
	mu      sync.Mutex
	hosts   map[string]*acphost.Host
	stopped bool
}

func NewManager(configs map[string]Config) (*Manager, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one harness is required")
	}
	for id, cfg := range configs {
		if id == "" || cfg.Command == "" {
			return nil, fmt.Errorf("harness id and command are required")
		}
		if _, err := permission.New(cfg.Permission); err != nil {
			return nil, fmt.Errorf("harness %q: %w", id, err)
		}
	}
	return &Manager{configs: configs, hosts: map[string]*acphost.Host{}}, nil
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
	session := &Session{at: at, id: id, generation: generation, host: host}
	if observe != nil {
		observe(at, session.Settings())
	}
	return session, nil
}

// SupportsHTTPMCP reports whether the harness's agent can take an HTTP MCP
// server in its session config.
func (m *Manager) SupportsHTTPMCP(ctx context.Context, at Placement) (bool, error) {
	host, err := m.host(at)
	if err != nil {
		return false, err
	}
	return host.SupportsHTTPMCP(ctx)
}

func (m *Manager) CloseSession(ctx context.Context, at Placement, upstreamID string) error {
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
	m.mu.Unlock()
	for _, host := range hosts {
		host.Close()
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
	for _, host := range m.hosts {
		hosts = append(hosts, host)
	}
	m.hosts = map[string]*acphost.Host{}
	m.mu.Unlock()
	for _, host := range hosts {
		host.Close()
	}
	return nil
}

func (m *Manager) host(at Placement) (*acphost.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, fmt.Errorf("harness manager is stopped")
	}
	key := at.key()
	if host := m.hosts[key]; host != nil {
		return host, nil
	}
	cfg, ok := m.configs[at.Harness]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", at.Harness)
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

type Media struct {
	MIME string
	Data []byte
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
}

type TurnRunner interface {
	Runner
	PromptTurn(context.Context, string, []Media, permission.AskFunc, acphost.AskUserFunc, func(view.Progress)) (string, []string, error)
}

type Session struct {
	at         Placement
	id         acp.SessionID
	generation uint64
	host       *acphost.Host
}

func (s *Session) ID() string { return string(s.id) }

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
	images := make([]acphost.Image, 0, len(media))
	for _, item := range media {
		if len(item.Data) == 0 {
			continue
		}
		images = append(images, acphost.Image{MIME: item.MIME, Data: item.Data})
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

func (s *Session) Cancel(ctx context.Context) error { return s.host.Cancel(ctx, s.id, s.generation) }

func (s *Session) Abort() {
	// The ACP subprocess is the cancellation boundary; use per-session
	// processes if cross-session disruption matters.
	s.host.Abort(s.generation)
}

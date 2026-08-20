package harness

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/permission"
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

type Manager struct {
	configs map[string]Config
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

func (m *Manager) OpenSession(ctx context.Context, harnessID, upstreamID, workdir string, servers []acp.MCPServer) (Runner, error) {
	if workdir == "" {
		return nil, fmt.Errorf("agent workspace is required")
	}
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return nil, fmt.Errorf("create agent workspace: %w", err)
	}
	host, err := m.host(harnessID)
	if err != nil {
		return nil, err
	}
	id, generation, err := host.OpenSession(ctx, acp.SessionID(upstreamID), acphost.SessionConfig{Workdir: workdir, MCPServers: servers})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		host.Close()
		return nil, fmt.Errorf("harness manager is stopped")
	}
	return &Session{harnessID: harnessID, id: id, generation: generation, host: host}, nil
}

func (m *Manager) CloseSession(ctx context.Context, harnessID, upstreamID string) error {
	m.mu.Lock()
	host := m.hosts[harnessID]
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

func (m *Manager) host(id string) (*acphost.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, fmt.Errorf("harness manager is stopped")
	}
	if host := m.hosts[id]; host != nil {
		return host, nil
	}
	cfg, ok := m.configs[id]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", id)
	}
	broker, err := permission.New(cfg.Permission)
	if err != nil {
		return nil, err
	}
	host := acphost.New(acphost.Config{
		Command: cfg.Command, Args: cfg.Args, ProcessDir: cfg.ProcessDir, Env: cfg.Env, Permission: broker,
	})
	m.hosts[id] = host
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
	Prompt(context.Context, string, func(card.Progress)) (string, []string, error)
	Cancel(context.Context) error
	Abort()
}

type TurnRunner interface {
	Runner
	PromptTurn(context.Context, string, []Media, permission.AskFunc, func(card.Progress)) (string, []string, error)
}

type Session struct {
	harnessID  string
	id         acp.SessionID
	generation uint64
	host       *acphost.Host
}

func (s *Session) ID() string { return string(s.id) }

func (s *Session) Prompt(ctx context.Context, text string, progress func(card.Progress)) (string, []string, error) {
	return s.PromptTurn(ctx, text, nil, nil, progress)
}

func (s *Session) PromptTurn(
	ctx context.Context,
	text string,
	media []Media,
	ask permission.AskFunc,
	progress func(card.Progress),
) (string, []string, error) {
	images := make([]acphost.Image, 0, len(media))
	for _, item := range media {
		if len(item.Data) == 0 {
			continue
		}
		images = append(images, acphost.Image{MIME: item.MIME, Data: item.Data})
	}
	return s.host.PromptTurn(ctx, s.id, s.generation, text, images, ask, progress)
}

func (s *Session) Cancel(ctx context.Context) error { return s.host.Cancel(ctx, s.id, s.generation) }

func (s *Session) Abort() {
	// ponytail: the ACP subprocess is the cancellation boundary; use per-session processes if cross-session disruption matters.
	s.host.Abort(s.generation)
}

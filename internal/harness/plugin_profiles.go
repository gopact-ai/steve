package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plugins"
)

// PluginRuntimeProvider returns a frozen node-local launch and secret-free
// MCP descriptors. Remote processes are launched by their node transport.
type PluginRuntimeProvider interface {
	PluginRuntime(context.Context, Placement, plugins.RuntimeRef) (Config, []acp.MCPServer, error)
}

type PluginTransports interface {
	PluginTransport(node, harness string, ref plugins.RuntimeRef) acphost.Transport
}

type pluginProfileKey struct{}

func WithPluginProfile(ctx context.Context, ref *plugins.RuntimeRef) context.Context {
	if ref == nil {
		return ctx
	}
	return context.WithValue(ctx, pluginProfileKey{}, ref.Clone())
}
func PluginProfile(ctx context.Context) *plugins.RuntimeRef {
	ref, _ := ctx.Value(pluginProfileKey{}).(*plugins.RuntimeRef)
	return ref.Clone()
}

func (m *Manager) SetPluginRuntimes(provider PluginRuntimeProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginRuntimes = provider
}

func profileSessionID(ref plugins.RuntimeRef, id acp.SessionID) string {
	return "ps_" + ref.ID + ":" + string(id)
}
func nativeProfileSessionID(ref plugins.RuntimeRef, id string) (acp.SessionID, error) {
	if id == "" {
		return "", nil
	}
	prefix := "ps_" + ref.ID + ":"
	raw, ok := strings.CutPrefix(id, prefix)
	if !ok || raw == "" {
		return "", errors.New("session belongs to another plugin runtime")
	}
	return acp.SessionID(raw), nil
}

func (m *Manager) openPluginSession(ctx context.Context, at Placement, upstream, workdir string, servers []acp.MCPServer) (Runner, error) {
	ref := PluginProfile(ctx)
	if ref == nil || ref.Validate() != nil || ref.Selection.Harness != at.Harness || ref.Selection.Node != at.Node {
		return nil, plugins.ErrInvalid
	}
	native, err := nativeProfileSessionID(*ref, upstream)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	provider := m.pluginRuntimes
	m.mu.Unlock()
	if provider == nil {
		return nil, errors.New("plugin runtime provider is unavailable")
	}
	cfg, extras, err := provider.PluginRuntime(ctx, at, *ref)
	if err != nil {
		return nil, err
	}
	combined := append(append([]acp.MCPServer(nil), servers...), extras...)
	host, err := m.pluginHost(at, *ref, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.pluginUsage(ctx, at, *ref, true); err != nil {
		return nil, err
	}
	id, generation, err := host.OpenSession(ctx, native, acphost.SessionConfig{Workdir: workdir, MCPServers: combined})
	if err != nil {
		host.Close()
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if stopErr := waitPluginProcessStop(cleanup, host); stopErr == nil {
			err = errors.Join(err, m.pluginUsage(cleanup, at, *ref, false))
		}
		return nil, err
	}
	m.mu.Lock()
	observe, stopped := m.observe, m.stopped
	m.mu.Unlock()
	if stopped {
		host.Close()
		return nil, errors.New("harness manager is stopped")
	}
	session := &Session{at: at, id: id, generation: generation, host: host, observe: observe, plugin: ref, pluginInstructions: cfg.PluginInstructions}
	if observe != nil {
		observe(at, session.Settings())
	}
	return session, nil
}

func (m *Manager) pluginHost(at Placement, ref plugins.RuntimeRef, cfg Config) (*acphost.Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.suspended["*"] || m.suspended[at.Node] {
		return nil, errors.New("harness manager is stopped or restarting")
	}
	key := "plugin/" + ref.ID + "/" + at.key()
	if host := m.hosts[key]; host != nil {
		return host, nil
	}
	policy := cfg.Permission
	if policy == "" {
		policy = permission.PolicyRead
	}
	broker, err := permission.New(policy)
	if err != nil {
		return nil, err
	}
	hostCfg := acphost.Config{Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, ProcessDir: cfg.ProcessDir, Permission: broker}
	if at.Node != "" {
		remote, ok := m.remote.(PluginTransports)
		if !ok {
			return nil, fmt.Errorf("%w: remote plugin transport unavailable", plugins.ErrIncompatible)
		}
		hostCfg = acphost.Config{Transport: remote.PluginTransport(at.Node, at.Harness, ref), Permission: broker}
	}
	host := acphost.New(hostCfg)
	m.hosts[key] = host
	if m.pluginRefs == nil {
		m.pluginRefs = map[*acphost.Host]plugins.RuntimeRef{}
	}
	m.pluginRefs[host] = *ref.Clone()
	return host, nil
}

func (m *Manager) closePluginSession(ctx context.Context, at Placement, id string) (bool, error) {
	if !strings.HasPrefix(id, "ps_") {
		return false, nil
	}
	head, native, ok := strings.Cut(strings.TrimPrefix(id, "ps_"), ":")
	if !ok || len(head) != 64 || native == "" {
		return true, plugins.ErrInvalid
	}
	m.mu.Lock()
	host := m.hosts["plugin/"+head+"/"+at.key()]
	m.mu.Unlock()
	if host == nil {
		return true, nil
	}
	if err := host.CloseSession(ctx, acp.SessionID(native)); err != nil {
		return true, err
	}
	host.Close()
	if err := waitPluginProcessStop(ctx, host); err != nil {
		return true, err
	}
	if ref := PluginProfile(ctx); ref != nil {
		if err := m.pluginUsage(ctx, at, *ref, false); err != nil {
			return true, err
		}
	}
	m.mu.Lock()
	delete(m.hosts, "plugin/"+head+"/"+at.key())
	delete(m.pluginRefs, host)
	provider := m.pluginRuntimes
	m.mu.Unlock()
	if closer, ok := provider.(PluginRuntimeCloser); ok {
		return true, closer.ClosePluginRuntime(ctx, at, head)
	}
	return true, nil
}

type PluginPreparation struct {
	AgentID   string
	At        Placement
	Project   string
	AttemptID string
	Upstream  string
	Prior     *plugins.RuntimeRef
}

type PluginSessionPreparer interface {
	PreparePluginSession(context.Context, PluginPreparation) (*plugins.RuntimeRef, error)
}

type PluginRelocationPreparer interface {
	PlanPluginRelocation(context.Context, PluginPreparation) (*plugins.Relocation, error)
	PreparePluginRelocation(context.Context, string, string, plugins.Relocation) (*plugins.RuntimeRef, error)
}

func (m *Manager) PlanPluginRelocation(ctx context.Context, req PluginPreparation) (*plugins.Relocation, error) {
	m.mu.Lock()
	provider, ok := m.pluginRuntimes.(PluginRelocationPreparer)
	m.mu.Unlock()
	if !ok {
		return nil, plugins.ErrUnavailable
	}
	return provider.PlanPluginRelocation(ctx, req)
}

func (m *Manager) PreparePluginRelocation(ctx context.Context, plan, id string, frozen plugins.Relocation) (*plugins.RuntimeRef, error) {
	m.mu.Lock()
	provider, ok := m.pluginRuntimes.(PluginRelocationPreparer)
	m.mu.Unlock()
	if !ok {
		return nil, plugins.ErrUnavailable
	}
	return provider.PreparePluginRelocation(ctx, plan, id, frozen)
}

func (m *Manager) PreparePluginSession(ctx context.Context, request PluginPreparation) (*plugins.RuntimeRef, error) {
	m.mu.Lock()
	provider := m.pluginRuntimes
	m.mu.Unlock()
	if provider == nil {
		if request.Prior != nil {
			return nil, errors.New("plugin runtime provider is unavailable")
		}
		return nil, nil
	}
	preparer, ok := provider.(PluginSessionPreparer)
	if !ok {
		return nil, errors.New("plugin runtime provider cannot prepare sessions")
	}
	return preparer.PreparePluginSession(ctx, request)
}

func (s *Session) PluginRuntime() *plugins.RuntimeRef { return s.plugin.Clone() }
func (s *managedSession) PluginRuntime() *plugins.RuntimeRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Plugin.Clone()
}

type PluginRuntimeCloser interface {
	ClosePluginRuntime(context.Context, Placement, string) error
}

func waitPluginProcessStop(ctx context.Context, host *acphost.Host) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !host.AllProcessesStopped() {
		select {
		case <-ctx.Done():
			return errors.Join(ErrStopUnconfirmed, ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

func (m *Manager) ClosePluginRuntime(ctx context.Context, ref plugins.RuntimeRef) error {
	at := Placement{Node: ref.Selection.Node, Harness: ref.Selection.Harness}
	key := "plugin/" + ref.ID + "/" + at.key()
	m.mu.Lock()
	host := m.hosts[key]
	provider := m.pluginRuntimes
	m.mu.Unlock()
	if host == nil {
		return nil
	}
	if err := host.CloseIdle(); err != nil {
		return err
	}
	if err := waitPluginProcessStop(ctx, host); err != nil {
		return err
	}
	if err := m.pluginUsage(ctx, at, ref, false); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.hosts, key)
	delete(m.pluginRefs, host)
	m.mu.Unlock()
	if closer, ok := provider.(PluginRuntimeCloser); ok {
		return closer.ClosePluginRuntime(ctx, at, ref.ID)
	}
	return nil
}

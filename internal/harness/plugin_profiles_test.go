package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/plugins"
)

// recordingPlugins is a plugin runtime provider that records each call.
type recordingPlugins struct{ calls []string }

func (p *recordingPlugins) PluginRuntime(context.Context, Placement, plugins.RuntimeRef) (Config, []acp.MCPServer, error) {
	p.calls = append(p.calls, "runtime")
	return Config{}, nil, nil
}
func (p *recordingPlugins) PreparePluginSession(context.Context, PluginPreparation) (*plugins.RuntimeRef, error) {
	p.calls = append(p.calls, "prepare")
	return &plugins.RuntimeRef{ID: "prepared"}, nil
}
func (p *recordingPlugins) PlanPluginRelocation(context.Context, PluginPreparation) (*plugins.Relocation, error) {
	p.calls = append(p.calls, "plan-relocation")
	return &plugins.Relocation{}, nil
}
func (p *recordingPlugins) PreparePluginRelocation(context.Context, string, string, plugins.Relocation) (*plugins.RuntimeRef, error) {
	p.calls = append(p.calls, "prepare-relocation")
	return &plugins.RuntimeRef{ID: "relocated"}, nil
}
func (p *recordingPlugins) ClosePluginRuntime(_ context.Context, _ Placement, id string) error {
	p.calls = append(p.calls, "close "+id)
	return nil
}
func (p *recordingPlugins) BeginPluginRuntimeUse(_ context.Context, _ Placement, ref plugins.RuntimeRef) error {
	p.calls = append(p.calls, "begin "+ref.ID)
	return nil
}
func (p *recordingPlugins) EndPluginRuntimeUse(_ context.Context, _ Placement, ref plugins.RuntimeRef) error {
	p.calls = append(p.calls, "end "+ref.ID)
	return nil
}

// idlePluginHost registers a plugin host that never started a process, so
// ClosePluginRuntime can close it without an agent.
func idlePluginHost(m *Manager, ref plugins.RuntimeRef) string {
	key := "plugin/" + ref.ID + "/" + Placement{Node: ref.Selection.Node, Harness: ref.Selection.Harness}.key()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hosts[key] = acphost.New(acphost.Config{})
	return key
}

// sessionRuntime is a runtime with a 64-character id; CloseSession refuses a
// plugin session id whose runtime id has any other length.
func sessionRuntime() plugins.RuntimeRef {
	return plugins.RuntimeRef{ID: strings.Repeat("a", 64), Selection: plugins.Selection{Harness: "mock"}}
}

func TestManagerWithoutPluginProviderRefusesOrSkipsPluginWork(t *testing.T) {
	m, err := NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	ref := plugins.RuntimeRef{ID: "runtime", Selection: plugins.Selection{Harness: "mock"}}
	if got, err := m.PreparePluginSession(ctx, PluginPreparation{}); got != nil || err != nil {
		t.Fatalf("prepare without prior = %v, %v; want nothing to prepare", got, err)
	}
	if got, err := m.PreparePluginSession(ctx, PluginPreparation{Prior: &ref}); got != nil || err == nil {
		t.Fatalf("prepare with prior = %v, %v; want an error", got, err)
	}
	if _, err := m.PlanPluginRelocation(ctx, PluginPreparation{Prior: &ref}); !errors.Is(err, plugins.ErrUnavailable) {
		t.Fatalf("plan relocation = %v, want ErrUnavailable", err)
	}
	if _, err := m.PreparePluginRelocation(ctx, "plan", "attempt", plugins.Relocation{}); !errors.Is(err, plugins.ErrUnavailable) {
		t.Fatalf("prepare relocation = %v, want ErrUnavailable", err)
	}
	for _, begin := range []bool{true, false} {
		if err := m.pluginUsage(ctx, Placement{Harness: "mock"}, ref, begin); err != nil {
			t.Fatalf("usage (begin=%v) = %v, want it skipped", begin, err)
		}
	}
	key := idlePluginHost(m, ref)
	if err := m.ClosePluginRuntime(ctx, ref); err != nil {
		t.Fatalf("close runtime = %v, want the host closed and nothing else", err)
	}
	if m.hosts[key] != nil {
		t.Fatal("closed runtime host is still registered")
	}
	session := sessionRuntime()
	key = idlePluginHost(m, session)
	if err := m.CloseSession(WithPluginProfile(ctx, &session), Placement{Harness: "mock"}, profileSessionID(session, "native")); err != nil {
		t.Fatalf("close session = %v, want the host closed and nothing else", err)
	}
	if m.hosts[key] != nil {
		t.Fatal("closed session host is still registered")
	}
}

func TestManagerHandsPluginWorkToItsProvider(t *testing.T) {
	m, err := NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingPlugins{}
	m.SetPluginRuntimes(provider)
	ctx := t.Context()
	ref := plugins.RuntimeRef{ID: "runtime", Selection: plugins.Selection{Harness: "mock"}}
	if got, err := m.PreparePluginSession(ctx, PluginPreparation{}); err != nil || got == nil || got.ID != "prepared" {
		t.Fatalf("prepare = %v, %v", got, err)
	}
	if _, err := m.PlanPluginRelocation(ctx, PluginPreparation{Prior: &ref}); err != nil {
		t.Fatalf("plan relocation = %v", err)
	}
	if got, err := m.PreparePluginRelocation(ctx, "plan", "attempt", plugins.Relocation{}); err != nil || got == nil || got.ID != "relocated" {
		t.Fatalf("prepare relocation = %v, %v", got, err)
	}
	if err := m.pluginUsage(ctx, Placement{Harness: "mock"}, ref, true); err != nil {
		t.Fatal(err)
	}
	idlePluginHost(m, ref)
	if err := m.ClosePluginRuntime(ctx, ref); err != nil {
		t.Fatal(err)
	}
	session := sessionRuntime()
	idlePluginHost(m, session)
	if err := m.CloseSession(WithPluginProfile(ctx, &session), Placement{Harness: "mock"}, profileSessionID(session, "native")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"prepare", "plan-relocation", "prepare-relocation", "begin runtime", "end runtime", "close runtime",
		"end " + session.ID, "close " + session.ID,
	}
	if !slices.Equal(provider.calls, want) {
		t.Fatalf("provider calls = %v, want %v", provider.calls, want)
	}
}

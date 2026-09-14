package turn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

type selectorRuntime struct {
	*fakeManager
	runner  harness.Runner
	failure error
	closed  []string
}

type cancelingSelector struct {
	*recoveryConfigurable
	cancel context.CancelFunc
}

func (r *cancelingSelector) Settings() view.Settings {
	r.cancel()
	return r.recoveryConfigurable.Settings()
}

func (m *selectorRuntime) OpenSession(ctx context.Context, at harness.Placement, upstream, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	if strings.HasPrefix(upstream, "ns_") {
		return nil, fmt.Errorf("%w: committed execution binding is required", harness.ErrNodeSessionUnavailable)
	}
	if m.failure != nil {
		return nil, m.failure
	}
	if _, err := m.fakeManager.OpenSession(ctx, at, upstream, workdir, servers); err != nil {
		return nil, err
	}
	return m.runner, nil
}

func (m *selectorRuntime) CloseSession(ctx context.Context, _ harness.Placement, id string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.closed = append(m.closed, id)
	return nil
}

func selectorCoordinator(t *testing.T, configurable bool) (*Coordinator, *selectorRuntime, *fakeRunner) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{"grok": {Harness: "grok", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	rt := &selectorRuntime{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"grok": runner}}, runner: runner}
	if configurable {
		rt.runner = &recoveryConfigurable{fakeRunner: runner, settings: view.Settings{Model: "m1", Options: []view.Option{{ID: "reasoning", Current: "low", Choices: []view.Choice{{Value: "low"}, {Value: "high"}}}}}}
	}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), rt, time.Minute)
	if err := store.SaveSession(state.Session{ConversationID: "chat", AgentID: "grok", HarnessID: "grok", UpstreamID: "ns_retained", Workspace: workspaceOf(t, c, "grok"), Tainted: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPreferences("chat", "grok", map[string]string{"model": "m2", "reasoning": "high"}); err != nil {
		t.Fatal(err)
	}
	return c, rt, runner
}

func TestSelectorsDiscoverWithoutResumingConversation(t *testing.T) {
	for _, configurable := range []bool{true, false} {
		t.Run(fmt.Sprint(configurable), func(t *testing.T) {
			c, rt, runner := selectorCoordinator(t, configurable)
			before := c.store.Conversation("chat")
			got, err := c.Selectors(t.Context(), "chat", "grok")
			if err != nil {
				t.Fatal(err)
			}
			if configurable && (got.Model != "m2" || len(got.Models) != 2 || got.Models[1].Value != "m2" || len(got.Options) != 1 || got.Options[0].Current != "high") {
				t.Fatalf("selectors lost choices or preferences: %+v", got)
			}
			if !configurable && !reflect.DeepEqual(got, Selectors{}) {
				t.Fatalf("invented selectors: %+v", got)
			}
			if !reflect.DeepEqual(rt.opened, []string{"grok:"}) || !reflect.DeepEqual(rt.closed, []string{runner.ID()}) {
				t.Fatalf("discovery must open and close only a fresh session: opened=%v closed=%v", rt.opened, rt.closed)
			}
			if len(runner.seen()) != 0 || len(rt.servers[0]) != 0 {
				t.Fatal("selector discovery sent work or exposed MCP tools")
			}
			if !reflect.DeepEqual(before, c.store.Conversation("chat")) {
				t.Fatal("selector discovery changed the retained conversation")
			}
		})
	}
}

func TestSelectorsUnavailableTargetReportsDiscoveryFailureAndCanRetry(t *testing.T) {
	for _, reason := range []string{"grok executable not found", "node disconnected"} {
		t.Run(reason, func(t *testing.T) {
			c, rt, runner := selectorCoordinator(t, true)
			before := c.store.Conversation("chat")
			rt.failure = errors.New(reason)
			_, err := c.Selectors(t.Context(), "chat", "grok")
			var userErr UserError
			if !errors.As(err, &userErr) || !strings.Contains(err.Error(), reason) || strings.Contains(err.Error(), "committed execution") {
				t.Fatalf("expected actionable target failure, got %v", err)
			}
			if !reflect.DeepEqual(before, c.store.Conversation("chat")) || len(runner.seen()) != 0 {
				t.Fatal("failed discovery changed conversation or sent work")
			}
			rt.failure = nil
			if _, err := c.Selectors(t.Context(), "chat", "grok"); err != nil {
				t.Fatalf("selector retry remained blocked after target returned: %v", err)
			}
		})
	}
}

func TestSelectorsCloseDiscoveryAfterRequestCancellation(t *testing.T) {
	c, rt, runner := selectorCoordinator(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt.runner = &cancelingSelector{recoveryConfigurable: rt.runner.(*recoveryConfigurable), cancel: cancel}
	if _, err := c.Selectors(ctx, "chat", "grok"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt.closed, []string{runner.ID()}) {
		t.Fatalf("canceled request leaked discovery session: closed=%v", rt.closed)
	}
}

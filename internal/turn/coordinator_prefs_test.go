package turn

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

func TestPreferenceResetAppliesConfiguredDefaultWithoutChangingContext(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	if err := c.catalog.Set("grok", agent.Config{Harness: "grok", Default: true, Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	rt.runner.(*recoveryConfigurable).settings.Model = "m2"
	selected, _ := c.catalog.Resolve("grok")
	if selected.Model != "m1" {
		t.Fatalf("fixture default = %q", selected.Model)
	}
	before := c.store.Conversation("chat").Sessions
	if _, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.open(t.Context(), state.Session{ConversationID: "chat", UpstreamID: "sess-1"}, selected, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if rt.runner.(*recoveryConfigurable).Settings().Model != "m1" {
		t.Fatal("reset retained the previous explicit model")
	}
	if !reflect.DeepEqual(before, c.store.Conversation("chat").Sessions) || len(rt.closed) > 0 {
		t.Fatal("reset replaced native context")
	}
}

func TestPreferenceResetWithoutKnownDefaultDoesNotPretendToSucceed(t *testing.T) {
	c, _, _ := selectorCoordinator(t, true)
	if err := c.store.SetPreferences("chat", "grok", map[string]string{"unknown-option": "chosen"}); err != nil {
		t.Fatal(err)
	}
	before := c.store.Conversation("chat")
	if _, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"unknown-option": ""}); err == nil {
		t.Fatal("reset without a declared default silently succeeded")
	}
	if !reflect.DeepEqual(before, c.store.Conversation("chat")) {
		t.Fatal("refused reset changed preferences")
	}
}

type orderedPreferenceRunner struct {
	*recoveryConfigurable
	mu      sync.Mutex
	value   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *orderedPreferenceRunner) SetModel(_ context.Context, _ string, value string) error {
	if value == "m1" {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	r.mu.Lock()
	r.value = value
	r.mu.Unlock()
	return nil
}

func TestConcurrentPreferencesKeepSavedAndLiveOrder(t *testing.T) {
	t.Parallel()
	c, rt, _ := selectorCoordinator(t, true)
	r := &orderedPreferenceRunner{recoveryConfigurable: rt.runner.(*recoveryConfigurable),
		entered: make(chan struct{}), release: make(chan struct{})}
	c.beginTurn("chat", "grok", func() {})
	c.setRunner("chat", "grok", r)
	first := make(chan error, 1)
	go func() {
		_, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": "m1"})
		first <- err
	}()
	<-r.entered
	second := make(chan error, 1)
	go func() {
		_, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": "m2"})
		second <- err
	}()
	// Release after the second call has had a chance to update the store.
	// Without serialization its live RPC completes before the first RPC.
	time.AfterFunc(time.Second, func() { close(r.release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if saved := c.store.Preferences("chat", "grok")["model"]; saved != r.value {
		t.Fatalf("saved model %q differs from live model %q", saved, r.value)
	}
}

// A resumed session comes back in the mode the harness policy implies,
// because the host re-applies that mode on every load. The owner's
// choice for this conversation must win again, or an approval mode picked
// in the composer silently reverts to "ask" after a restart.
func TestOpenReappliesOptionPreferencesToResumedSession(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	configurable := rt.runner.(*recoveryConfigurable)
	configurable.settings.Options = append(configurable.settings.Options, view.Option{
		ID: "mode", Category: "mode", Current: "read-only",
		Choices: []view.Choice{{Value: "read-only"}, {Value: "agent"}, {Value: "agent-full-access"}},
	})
	if err := c.store.SetPreferences("chat", "grok", map[string]string{"mode": "agent-full-access"}); err != nil {
		t.Fatal(err)
	}
	selected, _ := c.catalog.Resolve("grok")
	saved := state.Session{ConversationID: "chat", AgentID: "grok", HarnessID: "grok", UpstreamID: "sess-1"}
	if _, err := c.open(t.Context(), saved, selected, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range configurable.Settings().Options {
		got[o.ID] = o.Current
	}
	if got["mode"] != "agent-full-access" {
		t.Fatalf("resumed session mode = %q, want the conversation's preferred agent-full-access", got["mode"])
	}
	if got["reasoning"] != "high" {
		t.Fatalf("resumed session reasoning = %q, want preferred high", got["reasoning"])
	}
	if configurable.Settings().Model != "m2" {
		t.Fatalf("resume must apply the conversation model %q, got %q", "m2", configurable.Settings().Model)
	}
}

func TestApprovalDefaultPrecedenceWhenResuming(t *testing.T) {
	for _, tc := range []struct {
		name, global, agentMode, conversationMode, want string
	}{
		{"tool default", "", "", "", "read-only"},
		{"global default", "auto", "", "", "agent"},
		{"Agent overrides global", "full", "read-only", "", "read-only"},
		{"conversation overrides both", "ask", "agent", "agent-full-access", "agent-full-access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rt, _ := selectorCoordinator(t, true)
			configurable := rt.runner.(*recoveryConfigurable)
			configurable.settings.Options = append(configurable.settings.Options, view.Option{
				ID: "mode", Category: "mode", Current: "read-only",
				Choices: []view.Choice{{Value: "read-only"}, {Value: "agent"}, {Value: "agent-full-access"}},
			})
			selected, _ := c.catalog.Resolve("grok")
			selected.Approval = tc.global
			selected.Options = map[string]string{}
			if tc.agentMode != "" {
				selected.Options["mode"] = tc.agentMode
			}
			if tc.conversationMode != "" {
				if err := c.store.SetPreferences("chat", "grok", map[string]string{"mode": tc.conversationMode}); err != nil {
					t.Fatal(err)
				}
			}
			saved := state.Session{ConversationID: "chat", AgentID: "grok", HarnessID: "grok", UpstreamID: "sess-1"}
			if _, err := c.open(t.Context(), saved, selected, t.TempDir(), nil); err != nil {
				t.Fatal(err)
			}
			for _, option := range configurable.Settings().Options {
				if option.ID == "mode" && option.Current != tc.want {
					t.Fatalf("approval = %q, want %q", option.Current, tc.want)
				}
			}
			if selected.Options["mode"] != tc.agentMode || c.store.Preferences("chat", "grok")["mode"] != tc.conversationMode {
				t.Fatal("applying defaults mutated an Agent or conversation override")
			}
		})
	}
}

// Preferences change how the next turn runs, never which conversation it remembers.
func TestPreferencesPreserveContextAcrossIdleAndBusyUpdates(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, same := range []bool{false, true} {
			t.Run(fmt.Sprintf("busy=%v/same=%v", busy, same), func(t *testing.T) {
				c, rt, _ := selectorCoordinator(t, true)
				before := c.store.Conversation("chat")
				if busy {
					c.beginTurn("chat", "grok", func() {})
				}
				value := "m1"
				if same {
					value = "m2"
				}
				if _, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": value}); err != nil {
					t.Fatal(err)
				}
				after := c.store.Conversation("chat")
				if !reflect.DeepEqual(before.Sessions, after.Sessions) || !reflect.DeepEqual(before.Archived, after.Archived) || len(rt.closed) != 0 {
					t.Fatal("saving preferences replaced or scheduled replacement of native context")
				}
				if after.Preferences["grok"]["model"] != value {
					t.Fatal("preference was not recorded")
				}
			})
		}
	}
}

func TestResumedSessionAppliesExplicitModelPreference(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	selected, _ := c.catalog.Resolve("grok")
	saved := state.Session{ConversationID: "chat", AgentID: "grok", HarnessID: "grok", UpstreamID: "sess-1"}
	if _, err := c.open(t.Context(), saved, selected, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if rt.runner.(*recoveryConfigurable).Settings().Model != "m2" {
		t.Fatal("resumed context ignored explicitly selected model")
	}
}

func TestLivePreferencesNeverScheduleContextReplacement(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprint(refused), func(t *testing.T) {
			c, rt, _ := selectorCoordinator(t, true)
			r := rt.runner.(*recoveryConfigurable)
			r.refused = refused
			c.beginTurn("chat", "grok", func() {})
			c.setRunner("chat", "grok", r)
			before := c.store.Conversation("chat")
			live, err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": "m1"})
			if err != nil || live == refused {
				t.Fatalf("live=%v err=%v", live, err)
			}
			after := c.store.Conversation("chat")
			if !reflect.DeepEqual(before.Sessions, after.Sessions) || len(rt.closed) > 0 {
				t.Fatal("live preference changed context")
			}
		})
	}
}

func TestRefusedExplicitPreferencesBlockInputWithoutLosingContext(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	rt.runner.(*recoveryConfigurable).refused = true
	before := c.store.Conversation("chat")
	selected, _ := c.catalog.Resolve("grok")
	turn := &chatTurn{c: c, req: Request{ConversationID: "chat"}, selected: selected}
	if _, err := turn.arm(t.Context(), &lifecycle.Execution{Session: rt.runner, Record: attempt.Record{}}); err == nil {
		t.Fatal("unapplied preferences accepted")
	}
	if !reflect.DeepEqual(before, c.store.Conversation("chat")) {
		t.Fatal("refusal changed native session")
	}
	if len(rt.runner.(*recoveryConfigurable).seen()) > 0 {
		t.Fatal("refusal sent input")
	}
}

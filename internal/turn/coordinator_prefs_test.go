package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

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
	if configurable.Settings().Model != "m1" {
		t.Fatalf("resume must keep the session's model %q, got %q", "m1", configurable.Settings().Model)
	}
}

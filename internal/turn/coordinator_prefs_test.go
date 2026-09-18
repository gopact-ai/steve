package turn

import (
	"reflect"
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

// Changing a selector while the agent is answering is not refused. The
// running turn keeps the session it started on — taking it away mid-answer
// is the one thing worth refusing for — and the choice is held for the
// session the next turn opens.
func TestSelectorChosenDuringATurnLandsOnTheNextOne(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	if !c.beginTurn("chat", "grok", func() {}) {
		t.Fatal("the fixture already had a turn in flight")
	}
	if err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": "m3"}); err != nil {
		t.Fatalf("a choice made while the agent was answering was refused: %v", err)
	}
	during := c.store.Conversation("chat")
	if during.Preferences["grok"]["model"] != "m3" {
		t.Fatalf("the choice was not recorded: %+v", during.Preferences)
	}
	if len(rt.closed) != 0 {
		t.Fatalf("the running turn's session was closed under it: %v", rt.closed)
	}
	if _, ok := during.Sessions["grok"]; !ok {
		t.Fatal("the session record went away while a turn was still on it")
	}
	if !during.Renew["grok"] {
		t.Fatal("the next turn was not asked to open a fresh session")
	}

	// What the next turn does before it takes the session.
	c.clearActive("chat", "grok")
	if err := c.renewIfAsked(t.Context(), "chat", "grok"); err != nil {
		t.Fatal(err)
	}
	after := c.store.Conversation("chat")
	if _, ok := after.Sessions["grok"]; ok {
		t.Fatal("the next turn would have reused the session chosen against")
	}
	if !reflect.DeepEqual(rt.closed, []string{"ns_retained"}) {
		t.Fatalf("the replaced upstream session was left open: %v", rt.closed)
	}
	if len(after.Archived) != 1 {
		t.Fatalf("the replaced session is out of reach: %+v", after.Archived)
	}
	if after.Renew["grok"] {
		t.Fatal("the renewal was not spent, so every later turn would open a new session")
	}
	// Nothing left to renew: a second pass must not close anything again.
	if err := c.renewIfAsked(t.Context(), "chat", "grok"); err != nil {
		t.Fatal(err)
	}
	if len(rt.closed) != 1 {
		t.Fatalf("a spent renewal ran again: %v", rt.closed)
	}
}

// Between turns the session is exchanged straight away, as it always was.
func TestSelectorChosenBetweenTurnsRollsTheSessionAtOnce(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	if err := c.SetPreferences(t.Context(), "chat", "grok", map[string]string{"model": "m3"}); err != nil {
		t.Fatal(err)
	}
	after := c.store.Conversation("chat")
	if _, ok := after.Sessions["grok"]; ok || !reflect.DeepEqual(rt.closed, []string{"ns_retained"}) {
		t.Fatalf("an idle conversation did not roll its session over: sessions=%+v closed=%v", after.Sessions, rt.closed)
	}
	if after.Renew["grok"] {
		t.Fatal("a session already rolled over must not be renewed again")
	}
}

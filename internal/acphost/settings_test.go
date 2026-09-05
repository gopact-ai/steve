package acphost

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

// codexNewSession and claudeNewSession are trimmed captures of what the two
// adapters actually answer session/new with. Hand-written fixtures would
// only prove the resolver agrees with my reading of the spec; these prove it
// agrees with the agents.
const codexNewSession = `{
  "sessionId": "01a03e52-600e-7b52-9cf3-bf4745ef616c",
  "modes": {
    "currentModeId": "agent",
    "availableModes": [
      {"id": "read-only", "name": "Read-only"},
      {"id": "agent", "name": "Agent"},
      {"id": "agent-full-access", "name": "Agent (full access)"}
    ]
  },
  "configOptions": [
    {"id": "mode", "name": "Mode", "category": "mode", "type": "select",
     "currentValue": "agent",
     "options": [
       {"value": "read-only", "name": "Read-only"},
       {"value": "agent", "name": "Agent"},
       {"value": "agent-full-access", "name": "Agent (full access)"}
     ]},
    {"id": "model", "name": "Model", "category": "model", "type": "select",
     "currentValue": "gpt-5.6-sol",
     "options": [
       {"value": "gpt-5.6-sol", "name": "GPT 5.6 Sol"},
       {"value": "gpt-5.6-terra", "name": "GPT 5.6 Terra"}
     ]}
  ]
}`

const claudeNewSession = `{
  "sessionId": "a8ee09d2-532c-429b-b7a5-a64b0db633e1",
  "modes": {
    "currentModeId": "auto",
    "availableModes": [
      {"id": "auto", "name": "Auto"},
      {"id": "default", "name": "Manual"},
      {"id": "plan", "name": "Plan Mode"}
    ]
  },
  "configOptions": [
    {"id": "mode", "name": "Mode", "category": "mode", "type": "select",
     "currentValue": "auto",
     "options": [
       {"value": "auto", "name": "Auto"},
       {"value": "default", "name": "Manual"},
       {"value": "plan", "name": "Plan Mode"}
     ]},
    {"id": "model", "name": "Model", "category": "model", "type": "select",
     "currentValue": "opus[1m]",
     "options": [
       {"value": "default", "name": "Default (recommended)"},
       {"value": "sonnet", "name": "Sonnet"},
       {"value": "opus[1m]", "name": "Opus 5 (1M context)"}
     ]},
    {"id": "fast", "name": "Fast mode", "category": "model_config", "type": "select",
     "currentValue": "off",
     "options": [{"value": "on", "name": "On"}, {"value": "off", "name": "Off"}]}
  ]
}`

func stateFrom(t *testing.T, raw string) *sessionState {
	t.Helper()
	var resp acp.NewSessionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	return newSessionState(resp.Modes, resp.ConfigOptions)
}

func TestSettingsFromRealAdapters(t *testing.T) {
	for _, tc := range []struct {
		name, raw, model, mode string
	}{
		{"codex", codexNewSession, "GPT 5.6 Sol", "Agent"},
		{"claude", claudeNewSession, "Opus 5 (1M context)", "Auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stateFrom(t, tc.raw).settings()
			if got.Model != tc.model {
				t.Errorf("model = %q, want %q", got.Model, tc.model)
			}
			if got.Mode != tc.mode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.mode)
			}
			// The offered models come along, so a fleet view can show
			// what this harness could run without a config saying so.
			if len(got.Models) < 2 {
				t.Errorf("models = %v, want the adapter's list", got.Models)
			}
			if got.Harness != "" {
				t.Errorf("harness = %q, want empty: the host does not know it", got.Harness)
			}
		})
	}
}

func TestConfigOptionUpdateReplacesModel(t *testing.T) {
	state := stateFrom(t, claudeNewSession)
	raw := `{"sessionUpdate":"config_option_update","configOptions":[
	  {"id":"model","name":"Model","category":"model","type":"select",
	   "currentValue":"sonnet",
	   "options":[{"value":"sonnet","name":"Sonnet"},{"value":"opus[1m]","name":"Opus 5 (1M context)"}]}]}`
	var u acp.SessionUpdate
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	state.setOptions(u.ConfigOptions)
	if got := state.settings().Model; got != "Sonnet" {
		t.Fatalf("model after switch = %q, want %q", got, "Sonnet")
	}
}

func TestCurrentModeUpdateMovesMode(t *testing.T) {
	state := stateFrom(t, codexNewSession)
	state.setMode("read-only")
	if got := state.settings().Mode; got != "Read-only" {
		t.Fatalf("mode = %q, want %q", got, "Read-only")
	}
}

// An option list that drops the mode selector must not drop the mode: the
// two carry it on separate channels and modeID is the single answer.
func TestOptionsWithoutModeKeepCurrentMode(t *testing.T) {
	state := stateFrom(t, codexNewSession)
	state.setOptions([]acp.SessionConfigOption{{
		Type: acp.SessionConfigOptionTypeSelect, ID: "model", Name: "Model",
		CurrentValue: acp.SessionConfigValueID("gpt-5.6-terra"),
	}})
	got := state.settings()
	if got.Mode != "Agent" {
		t.Errorf("mode = %q, want it kept as %q", got.Mode, "Agent")
	}
	// No options list means no name to look up, so the ID stands in.
	if got.Model != "gpt-5.6-terra" {
		t.Errorf("model = %q, want the raw id", got.Model)
	}
}

// Category is advisory in ACP; an agent may omit it entirely.
func TestFindOptionFallsBackToID(t *testing.T) {
	state := &sessionState{}
	state.setOptions([]acp.SessionConfigOption{{
		Type: acp.SessionConfigOptionTypeSelect, ID: "model", Name: "Model",
		CurrentValue: acp.SessionConfigValueID("m1"),
		Options: acp.SessionConfigSelectOptions{
			Ungrouped: &acp.UngroupedSessionConfigSelectOptions{{Value: "m1", Name: "Model One"}},
		},
	}})
	if got := state.settings().Model; got != "Model One" {
		t.Fatalf("model = %q, want %q", got, "Model One")
	}
}

func TestGroupedOptionsResolveNames(t *testing.T) {
	category := acp.SessionConfigOptionCategoryModel
	state := &sessionState{}
	state.setOptions([]acp.SessionConfigOption{{
		Type: acp.SessionConfigOptionTypeSelect, ID: "m", Name: "Model", Category: &category,
		CurrentValue: acp.SessionConfigValueID("b2"),
		Options: acp.SessionConfigSelectOptions{
			Groups: &acp.GroupedSessionConfigSelectOptions{
				{Group: "a", Name: "A", Options: []acp.SessionConfigSelectOption{{Value: "a1", Name: "A One"}}},
				{Group: "b", Name: "B", Options: []acp.SessionConfigSelectOption{{Value: "b2", Name: "B Two"}}},
			},
		},
	}})
	if got := state.settings().Model; got != "B Two" {
		t.Fatalf("model = %q, want %q", got, "B Two")
	}
}

// A boolean selector has no name table; asking for its label must not panic
// or invent one.
func TestBooleanOptionYieldsNoModel(t *testing.T) {
	category := acp.SessionConfigOptionCategoryModel
	state := &sessionState{}
	state.setOptions([]acp.SessionConfigOption{{
		Type: acp.SessionConfigOptionTypeBoolean, ID: "m", Name: "Model",
		Category: &category, CurrentValue: true,
	}})
	if got := state.settings().Model; got != "" {
		t.Fatalf("model = %q, want empty", got)
	}
}

func TestNilSessionStateHasEmptySettings(t *testing.T) {
	var state *sessionState
	if got := state.settings(); !got.Empty() {
		t.Fatalf("settings = %+v, want empty", got)
	}
}

// The unit tests above resolve labels from decoded structs. This one runs a
// real agent subprocess over a real ACP connection, so it proves the wiring
// in between — session/new capture, notification routing, and the snapshot
// the card is drawn from — actually carries the model.
func TestSettingsReachProgressOverTheWire(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Settings(sid); got.Model != "Mock Fast" || got.Mode != "Agent" {
		t.Fatalf("settings after session/new = %+v", got)
	}

	var seen []view.Settings
	if _, _, err := h.Prompt(t.Context(), sid, generation, "hello", func(p view.Progress) {
		seen = append(seen, p.Settings)
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("no progress snapshots")
	}
	for i, got := range seen {
		if got.Model != "Mock Fast" {
			t.Fatalf("snapshot %d model = %q, want %q", i, got.Model, "Mock Fast")
		}
	}
}

func TestConfigOptionUpdateReachesProgressMidTurn(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var last view.Settings
	if _, _, err := h.Prompt(t.Context(), sid, generation, "switchmodel please", func(p view.Progress) {
		last = p.Settings
	}); err != nil {
		t.Fatal(err)
	}
	if last.Model != "Mock Deep" {
		t.Fatalf("model after mid-turn switch = %q, want %q", last.Model, "Mock Deep")
	}
	// The switch is the session's now, not just this turn's.
	if got := h.Settings(sid).Model; got != "Mock Deep" {
		t.Fatalf("session model = %q, want %q", got, "Mock Deep")
	}
}

func TestPlanReachesProgress(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var last []view.Step
	if _, _, err := h.Prompt(t.Context(), sid, generation, "make a plan", func(p view.Progress) {
		if len(p.Plan) > 0 {
			last = p.Plan
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []view.Step{
		{Text: "look around", Status: view.StepCompleted},
		{Text: "do the thing", Status: view.StepInProgress},
		{Text: "check it", Status: view.StepPending},
	}
	if len(last) != len(want) {
		t.Fatalf("plan = %+v, want %d steps", last, len(want))
	}
	for i := range want {
		if last[i] != want[i] {
			t.Errorf("step %d = %+v, want %+v", i, last[i], want[i])
		}
	}
}

// A replayed user message must not be folded into the agent's answer.
func TestUserMessageChunkIsDropped(t *testing.T) {
	col := &collector{}
	col.handle(acp.UserMessageChunkSessionUpdate(acp.TextContentBlock("my own words")))
	col.handle(acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("the reply")))
	text, _ := col.result()
	if text != "the reply" {
		t.Fatalf("collected %q, want only the agent's own text", text)
	}
}

func TestSetOptionSwitchesModel(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	id, choices := h.ModelChoices(sid)
	if id != "model" {
		t.Fatalf("model option id = %q", id)
	}
	want := []view.Choice{{Value: "mock-fast", Label: "Mock Fast"}, {Value: "mock-deep", Label: "Mock Deep"}}
	if len(choices) != len(want) {
		t.Fatalf("choices = %+v", choices)
	}
	for i := range want {
		if choices[i] != want[i] {
			t.Errorf("choice %d = %+v, want %+v", i, choices[i], want[i])
		}
	}
	if err := h.SetOption(t.Context(), sid, generation, id, "mock-deep"); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings(sid).Model; got != "Mock Deep" {
		t.Fatalf("model after set = %q, want %q", got, "Mock Deep")
	}
}

// The agent stays the authority on what it is running: a refused change must
// not move Steve's record.
func TestSetOptionRefusedLeavesModelAlone(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetOption(t.Context(), sid, generation, "model", "no-such-model"); err == nil {
		t.Fatal("expected the agent to refuse an unknown model")
	}
	if got := h.Settings(sid).Model; got != "Mock Fast" {
		t.Fatalf("model = %q, want it unchanged", got)
	}
}

func TestListSessions(t *testing.T) {
	h := newTestHost(t, "deny")
	sessions, err := h.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "mock-session-1" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestDeleteSessionRemovesItFromTheAgent(t *testing.T) {
	h := newTestHost(t, "deny")
	if _, _, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	before, err := h.ListSessions(t.Context())
	if err != nil || len(before) != 1 {
		t.Fatalf("sessions before = %+v, err = %v", before, err)
	}
	if err := h.DeleteSession(t.Context(), "mock-session-1"); err != nil {
		t.Fatal(err)
	}
	after, err := h.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("sessions after delete = %+v", after)
	}
}

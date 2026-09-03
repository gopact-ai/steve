package agent

import "testing"

func TestCatalogResolveAlias(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{
		"claude": {Harness: "claude-code", Aliases: []string{"cc"}},
		"codex":  {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := catalog.Resolve("cc")
	if !ok || got.ID != "claude" {
		t.Fatalf("Resolve(cc) = %#v, %v", got, ok)
	}
	if catalog.Default().ID != "codex" {
		t.Fatalf("unexpected default: %#v", catalog.Default())
	}
}

func TestCatalogRejectsDuplicateAlias(t *testing.T) {
	_, err := NewCatalog(map[string]Config{
		"claude": {Harness: "claude", Aliases: []string{"agent"}},
		"codex":  {Harness: "codex", Aliases: []string{"agent"}, Default: true},
	})
	if err == nil {
		t.Fatal("expected duplicate alias error")
	}
}

func TestCatalogRejectsDuplicateNormalizedID(t *testing.T) {
	_, err := NewCatalog(map[string]Config{
		"Claude": {Harness: "one", Default: true},
		"claude": {Harness: "two"},
	})
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestSelectorParsesTagAndUse(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{
		"claude": {Harness: "claude", Aliases: []string{"cc"}},
		"codex":  {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		input      string
		agentID    string
		prompt     string
		switchOnly bool
	}{
		{name: "tag", input: "@cc fix it", agentID: "claude", prompt: "fix it"},
		{name: "use with prompt", input: "/use codex inspect", agentID: "codex", prompt: "inspect"},
		{name: "use only", input: "/use claude", agentID: "claude", switchOnly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selection, ok := catalog.Select(tt.input)
			if !ok || selection.Agent.ID != tt.agentID || selection.Prompt != tt.prompt || selection.SwitchOnly != tt.switchOnly {
				t.Fatalf("Select(%q) = %#v, %v", tt.input, selection, ok)
			}
		})
	}
}

func TestSelectorPreservesMultilinePrompt(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{"claude": {Harness: "claude", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	selection, ok := catalog.Select("@claude first line\n  second line")
	if !ok || selection.Prompt != "first line\n  second line" {
		t.Fatalf("unexpected selection: %#v, %v", selection, ok)
	}
}

func TestSelectorAcceptsWhitespaceSeparators(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{"claude": {Harness: "claude", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"@claude\nfix it", "/use\tclaude\nfix it"} {
		selection, ok := catalog.Select(input)
		if !ok || selection.Agent.ID != "claude" || selection.Prompt != "fix it" {
			t.Fatalf("Select(%q) = %#v, %v", input, selection, ok)
		}
	}
}

func TestSelectorParsesTagWithoutSpace(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{
		"claude": {Harness: "claude"},
		"codex":  {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		input   string
		agentID string
		prompt  string
	}{
		{input: "@codex帮我看看", agentID: "codex", prompt: "帮我看看"},
		{input: "@claude：帮我", agentID: "claude", prompt: "：帮我"},
		{input: "/use codexhello", agentID: "codex", prompt: "hello"},
	}
	for _, tt := range tests {
		selection, ok := catalog.Select(tt.input)
		if !ok || selection.Agent.ID != tt.agentID || selection.Prompt != tt.prompt || selection.SwitchOnly {
			t.Fatalf("Select(%q) = %#v, %v", tt.input, selection, ok)
		}
	}
}

func TestSelectorRejectsUnknownTag(t *testing.T) {
	catalog, err := NewCatalog(map[string]Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Select("@gemini fix it"); ok {
		t.Fatal("unknown tag was accepted")
	}
}

// An agent added at runtime is held to the same rules as one from the
// file: a duplicate id or alias is refused and leaves the catalog as it
// was; a good one resolves at once, and readers see whole catalogs only.
func TestCatalogAddsAnAgentAtRuntime(t *testing.T) {
	c, err := NewCatalog(map[string]Config{"codex": {Harness: "codex", Default: true, Aliases: []string{"cx"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Add("codex", Config{Harness: "codex"}); err == nil {
		t.Fatal("a duplicate id was accepted")
	}
	if err := c.Add("cx", Config{Harness: "codex"}); err == nil {
		t.Fatal("an id that is another agent's alias was accepted")
	}
	if err := c.Add("reviewer", Config{Harness: "claude-code", Node: "node-a"}); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Resolve("reviewer")
	if !ok || got.Node != "node-a" || got.Harness != "claude-code" {
		t.Fatalf("resolve after add = %+v, %v", got, ok)
	}
	if len(c.List()) != 2 || c.Default().ID != "codex" {
		t.Fatalf("list = %d, default = %s", len(c.List()), c.Default().ID)
	}
}

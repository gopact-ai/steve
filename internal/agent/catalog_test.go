package agent

import "testing"

func TestEmptyCatalogCanBeReadBeforeFirstRegistration(t *testing.T) {
	catalog, err := NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.List()) != 0 || catalog.Default().ID != "" {
		t.Fatal("empty catalog has a configured agent")
	}
	if _, ok := catalog.Resolve("codex"); ok {
		t.Fatal("empty catalog resolved an agent")
	}
	if _, ok := catalog.Select("@codex hello"); ok {
		t.Fatal("empty catalog selected an agent")
	}
	if err := catalog.Add("codex", Config{Harness: "codex", Default: true}); err != nil {
		t.Fatal(err)
	}
	if catalog.Default().ID != "codex" {
		t.Fatal("first registration was not published")
	}
}

func TestCatalogOwnsImmutableConfigurations(t *testing.T) {
	options := map[string]string{"effort": "high"}
	aliases := []string{"worker"}
	catalog, err := NewCatalog(map[string]Config{"main": {Harness: "mock", Default: true, Options: options, Aliases: aliases}})
	if err != nil {
		t.Fatal(err)
	}
	options["effort"], aliases[0] = "low", "changed"
	got, ok := catalog.Resolve("worker")
	if !ok || got.Options["effort"] != "high" || got.Aliases[0] != "worker" {
		t.Fatalf("caller changed catalog: %+v", got)
	}
	got.Options["effort"] = "low"
	listed := catalog.List()
	listed[0].Aliases[0] = "changed"
	if catalog.Default().Options["effort"] != "high" || catalog.Default().Aliases[0] != "worker" {
		t.Fatal("read results changed the published catalog")
	}
}

func TestCatalogPublishesPreparedSnapshot(t *testing.T) {
	live, err := NewCatalog(map[string]Config{"old": {Harness: "mock", Default: true, Aliases: []string{"worker"}}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := NewCatalog(map[string]Config{"new": {Harness: "mock", Default: true, Aliases: []string{"worker"}}})
	if err != nil {
		t.Fatal(err)
	}
	if live.Default().ID != "old" {
		t.Fatal("preparation changed live state")
	}
	live.Publish(prepared)
	if got, ok := live.Resolve("worker"); !ok || got.ID != "new" {
		t.Fatalf("published alias = %+v, %v", got, ok)
	}
	if err := prepared.Add("later", Config{Harness: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := live.Resolve("later"); ok {
		t.Fatal("candidate mutation leaked into published snapshot")
	}
	if err := live.Add("local", Config{Harness: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := prepared.Resolve("local"); ok {
		t.Fatal("published catalog shares mutable configs")
	}
	live.Publish(live)
	if live.Default().ID != "new" {
		t.Fatal("self publication changed catalog")
	}
}

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

// Set replaces an agent whole and Remove forgets one; the default agent
// cannot be removed, and a bad replacement leaves the catalog as it was.
func TestCatalogSetsAndRemovesAgents(t *testing.T) {
	c, err := NewCatalog(map[string]Config{"codex": {Harness: "codex", Default: true}, "builder": {Harness: "codex", Node: "node-a", Requires: []string{"gpu"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set("builder", Config{Harness: "claude-code", Node: "node-b", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Resolve("builder"); got.Harness != "claude-code" || got.Node != "node-b" || got.Model != "opus" || len(got.Requires) != 0 {
		t.Fatalf("after set = %+v", got)
	}
	if err := c.Set("builder", Config{}); err == nil {
		t.Fatal("an agent without a harness was accepted")
	}
	if got, _ := c.Resolve("builder"); got.Harness != "claude-code" {
		t.Fatalf("a refused set changed the catalog: %+v", got)
	}
	if err := c.Remove("codex"); err == nil {
		t.Fatal("the default agent was removed")
	}
	if err := c.Remove("builder"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Resolve("builder"); ok || len(c.List()) != 1 {
		t.Fatal("builder is still there")
	}
}

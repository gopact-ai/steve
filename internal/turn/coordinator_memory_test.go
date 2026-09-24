package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

const memoryOwner = "owner"

// memoryCoordinator has an owner, projects alpha and beta, and alpha as
// the default project.
func memoryCoordinator(t *testing.T, opts ...testOption) *Coordinator {
	t.Helper()
	c := buildCoordinator(t, append([]testOption{withOwner(memoryOwner), withDeps(func(d *Deps) { d.DefaultProject = "alpha" })}, opts...)...)
	if err := c.projects.Declare(t.Context(), []project.Project{
		{ID: "alpha", Home: project.Home{Path: t.TempDir()}},
		{ID: "beta", Home: project.Home{Path: t.TempDir()}},
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// arrive is a message reaching conversation from sender in chatType.
func arrive(c *Coordinator, conversation, sender string, chatType protocol.ChatType) {
	c.rememberMode(Request{ConversationID: conversation, SenderOpenID: sender, ChatType: chatType})
}

// seedFact writes text into scope directly, bypassing the agent rules.
func seedFact(t *testing.T, c *Coordinator, scope memory.Scope, text string) string {
	t.Helper()
	r, err := c.memory.Remember(t.Context(), scope, "", text, "", memory.Actor{By: "console"})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func factsIn(t *testing.T, c *Coordinator, scope memory.Scope) []string {
	t.Helper()
	items, err := c.memory.List(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Text)
	}
	return texts
}

func TestMemoryToolsRefuseConversationsNotLastReachedByTheOwnerInPrivate(t *testing.T) {
	type arrival struct {
		sender string
		chat   protocol.ChatType
	}
	cases := map[string][]arrival{
		"never seen":          nil,
		"owner in a group":    {{memoryOwner, protocol.ChatGroup}},
		"guest in private":    {{"guest", protocol.ChatP2P}},
		"owner then in group": {{memoryOwner, protocol.ChatP2P}, {memoryOwner, protocol.ChatGroup}},
	}
	for name, arrivals := range cases {
		t.Run(name, func(t *testing.T) {
			c := memoryCoordinator(t)
			for _, a := range arrivals {
				arrive(c, "chat", a.sender, a.chat)
			}
			id := seedFact(t, c, memory.Global, "prefers tabs")
			if _, _, err := c.Remember(t.Context(), "chat", "codex", "", "global", "", "likes go", ""); err == nil {
				t.Fatal("remember was allowed")
			}
			if hits, _, err := c.Recall(t.Context(), "chat", "codex", "", "tabs", 10); err == nil {
				t.Fatalf("recall was allowed: %+v", hits)
			}
			if _, err := c.Forget(t.Context(), "chat", "codex", "", "global", id); err == nil {
				t.Fatal("forget was allowed")
			}
			if got := factsIn(t, c, memory.Global); len(got) != 1 || got[0] != "prefers tabs" {
				t.Fatalf("refused calls changed memory: %q", got)
			}
		})
	}
}

func TestMemoryToolsServeTheOwnerLastSeenInPrivate(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatGroup)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	receipt, scope, err := c.Remember(t.Context(), "chat", "codex", "", "global", "", "prefers tabs", "")
	if err != nil || scope != memory.Global {
		t.Fatalf("remember: scope=%v err=%v", scope, err)
	}
	hits, _, err := c.Recall(t.Context(), "chat", "codex", "global", "tabs", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != receipt.ID {
		t.Fatalf("recall: %+v %v", hits, err)
	}
	if _, err := c.Forget(t.Context(), "chat", "codex", "", "global", receipt.ID); err != nil {
		t.Fatal(err)
	}
	if got := factsIn(t, c, memory.Global); len(got) != 0 {
		t.Fatalf("forget left %q", got)
	}
}

func TestMemoryToolsLetADelegatedTaskRecallButNotRememberOrForget(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	id := seedFact(t, c, memory.Global, "prefers tabs")
	if _, _, err := c.Remember(t.Context(), "chat", "codex", "parent", "global", "", "likes go", ""); err == nil {
		t.Fatal("delegated task remembered")
	}
	if _, err := c.Forget(t.Context(), "chat", "codex", "parent", "global", id); err == nil {
		t.Fatal("delegated task forgot")
	}
	if got := factsIn(t, c, memory.Global); len(got) != 1 || got[0] != "prefers tabs" {
		t.Fatalf("delegated task changed memory: %q", got)
	}
	hits, _, err := c.Recall(t.Context(), "chat", "codex", "global", "tabs", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("delegated recall: %+v %v", hits, err)
	}
}

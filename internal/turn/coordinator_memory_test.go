package turn

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
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

func TestMemoryToolsRefuseADelegatedTaskRememberAndForget(t *testing.T) {
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
}

// rememberInProject is the owner, in private, remembering into "project".
func rememberInProject(t *testing.T, ctx context.Context, c *Coordinator) (memory.Scope, error) {
	t.Helper()
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	_, scope, err := c.Remember(ctx, "chat", "codex", "", "project", "", "tests run with -race", "")
	return scope, err
}

func bind(t *testing.T, c *Coordinator, conversation, projectID string) {
	t.Helper()
	if _, err := c.projects.Bind(t.Context(), conversation, projectID, memoryOwner); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryProjectScopeIsTheBoundProjectElseTheDefault(t *testing.T) {
	c := memoryCoordinator(t)
	if scope, err := rememberInProject(t, t.Context(), c); err != nil || scope != memory.ProjectScope("alpha") {
		t.Fatalf("unbound: scope=%v err=%v", scope, err)
	}
	bind(t, c, "chat", "beta")
	if scope, err := rememberInProject(t, t.Context(), c); err != nil || scope != memory.ProjectScope("beta") {
		t.Fatalf("bound: scope=%v err=%v", scope, err)
	}
	if got := factsIn(t, c, memory.ProjectScope("beta")); len(got) != 1 {
		t.Fatalf("beta holds %q", got)
	}
}

func TestMemoryProjectScopeIsRefusedForSteveHome(t *testing.T) {
	cases := map[string]func(*Coordinator){
		"bound to home": func(c *Coordinator) { bind(t, c, "chat", "home") },
		// With a home project and an owner, an unbound conversation
		// falls to home rather than to the default project.
		"unbound with a home project": func(*Coordinator) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := memoryCoordinator(t)
			useHome(t, c, t.TempDir())
			setup(c)
			if scope, err := rememberInProject(t, t.Context(), c); err == nil {
				t.Fatalf("remembered into %v", scope)
			}
			if _, scope, err := c.Remember(t.Context(), "chat", "codex", "", "global", "", "prefers tabs", ""); err != nil || scope != memory.Global {
				t.Fatalf("global: scope=%v err=%v", scope, err)
			}
		})
	}
}

func TestMemoryToolsRefuseAScopeThatIsNotGlobalOrProject(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	seedFact(t, c, memory.Global, "prefers tabs")
	if _, scope, err := c.Remember(t.Context(), "chat", "codex", "", "team", "", "likes go", ""); err == nil {
		t.Fatalf("remembered into %v", scope)
	}
	if hits, _, err := c.Recall(t.Context(), "chat", "codex", "team", "tabs", 10); err == nil {
		t.Fatalf("recalled %+v", hits)
	}
	if got := factsIn(t, c, memory.Global); len(got) != 1 {
		t.Fatalf("global holds %q", got)
	}
}

func TestRecallInProjectScopeIsRefusedWithoutAProject(t *testing.T) {
	c := memoryCoordinator(t)
	useHome(t, c, t.TempDir())
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	seedFact(t, c, memory.ProjectScope("home"), "tabs at home")
	if hits, _, err := c.Recall(t.Context(), "chat", "codex", "project", "tabs", 10); err == nil {
		t.Fatalf("recalled %+v", hits)
	}
}

// inAttempt is ctx inside an execution of an attempt on projectID.
func inAttempt(t *testing.T, c *Coordinator, id, projectID string) context.Context {
	t.Helper()
	r := attempt.Record{Spec: attempt.Spec{ID: id, Kind: attempt.KindChat, Project: projectID}, State: attempt.Running}
	if _, err := ledgerOf(t, c).Begin(t.Context(), r.ID, "attempt", string(r.State), "", r); err != nil {
		t.Fatal(err)
	}
	return execution.WithProbeKey(t.Context(), execution.Key{AttemptID: id})
}

func TestMemoryProjectScopeInsideAnExecutionIsTheAttemptsProject(t *testing.T) {
	c := memoryCoordinator(t)
	useHome(t, c, t.TempDir())
	bind(t, c, "chat", "alpha")
	if scope, err := rememberInProject(t, inAttempt(t, c, "on-beta", "beta"), c); err != nil || scope != memory.ProjectScope("beta") {
		t.Fatalf("attempt on beta: scope=%v err=%v", scope, err)
	}
	if scope, err := rememberInProject(t, inAttempt(t, c, "on-home", "home"), c); err == nil {
		t.Fatalf("attempt on home remembered into %v", scope)
	}
}

// memoryCoordinator declares no home project. With one, as the application
// always declares, an unbound owner DM falls to home and gets none.
func TestProjectMemoryOfAnUnboundConversationWithoutAHomeProjectIsTheDefaultProjects(t *testing.T) {
	c := memoryCoordinator(t)
	seedFact(t, c, memory.ProjectScope("alpha"), "tests run with -race")
	owner := Request{ConversationID: "chat", SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P}
	if got := c.projectMemory(t.Context(), "chat", owner); len(got) != 1 || got[0].Name != "memory:project:alpha" {
		t.Fatalf("unbound conversation got %+v", got)
	}
}

func TestProjectMemoryIsInjectedForTheOwnerInPrivateWhenTheProjectHasAny(t *testing.T) {
	c := memoryCoordinator(t)
	bind(t, c, "chat", "beta")
	owner := Request{ConversationID: "chat", SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P}
	if got := c.projectMemory(t.Context(), "chat", owner); got != nil {
		t.Fatalf("empty project injected %+v", got)
	}
	seedFact(t, c, memory.ProjectScope("beta"), "tests run with -race")
	got := c.projectMemory(t.Context(), "chat", owner)
	if len(got) != 1 || got[0].Name != "memory:project:beta" || !strings.Contains(got[0].Memory, "tests run with -race") {
		t.Fatalf("owner in private got %+v", got)
	}
	for name, req := range map[string]Request{
		"owner in a group": {ConversationID: "chat", SenderOpenID: memoryOwner, ChatType: protocol.ChatGroup},
		"guest in private": {ConversationID: "chat", SenderOpenID: "guest", ChatType: protocol.ChatP2P},
	} {
		if got := c.projectMemory(t.Context(), "chat", req); got != nil {
			t.Fatalf("%s got %+v", name, got)
		}
	}
}

func hitTexts(hits []memory.Hit) []string {
	var texts []string
	for _, h := range hits {
		texts = append(texts, h.Text)
	}
	return texts
}

// The markdown store scores a fact by the share of query words it holds:
// with "tabs rust", a fact with both scores 1 and one with "tabs" 0.5.
func TestRecallWithoutScopeReturnsUpToLimitFromEachScopeGlobalFirst(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	seedFact(t, c, memory.Global, "tabs in docs")
	seedFact(t, c, memory.Global, "tabs for yaml")
	seedFact(t, c, memory.Global, "rust and tabs in go")
	seedFact(t, c, memory.ProjectScope("alpha"), "tabs in makefiles")
	seedFact(t, c, memory.ProjectScope("alpha"), "rust uses tabs")
	seedFact(t, c, memory.ProjectScope("alpha"), "tabs in ci")
	hits, _, err := c.Recall(t.Context(), "chat", "codex", "", "tabs rust", 2)
	if err != nil {
		t.Fatal(err)
	}
	// Each scope keeps its own ranking; scores are not compared across them.
	want := []string{"rust and tabs in go", "tabs in docs", "rust uses tabs", "tabs in makefiles"}
	if got := hitTexts(hits); !slices.Equal(got, want) {
		t.Fatalf("recalled %q, want %q", got, want)
	}
}

// projectIndex answers for projects only; global falls back to the store.
type projectIndex struct{}

func (projectIndex) Name() string                             { return "index" }
func (projectIndex) Index(context.Context, memory.Item) error { return nil }
func (projectIndex) Drop(context.Context, memory.Item) error  { return nil }
func (projectIndex) Recall(_ context.Context, scope memory.Scope, _ string, _ int) ([]memory.Hit, error) {
	if scope.Kind != memory.KindProject {
		return nil, errors.New("global is not indexed")
	}
	return []memory.Hit{{Item: memory.Item{ID: "p1", Scope: scope, Text: "indexed"}, Score: 1}}, nil
}

func TestRecallWithoutScopeNamesEachSourceItSearched(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	if _, from, err := c.Recall(t.Context(), "chat", "codex", "", "tabs", 10); err != nil || from != "markdown" {
		t.Fatalf("one source: from=%q err=%v", from, err)
	}
	dir := t.TempDir()
	svc := memory.NewService(memory.NewMarkdown(t.TempDir(), dir), filepath.Join(dir, "audit.jsonl"))
	svc.SetRetriever(projectIndex{})
	c = memoryCoordinator(t, withDeps(func(d *Deps) { d.Memory = svc }))
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	if _, from, err := c.Recall(t.Context(), "chat", "codex", "", "tabs", 10); err != nil || from != "markdown+index" {
		t.Fatalf("two sources: from=%q err=%v", from, err)
	}
}

func TestRecallWithoutScopeSearchesGlobalAndTheProject(t *testing.T) {
	c := memoryCoordinator(t)
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	bind(t, c, "chat", "beta")
	seedFact(t, c, memory.Global, "tabs in docs")
	seedFact(t, c, memory.ProjectScope("beta"), "tabs in makefiles")
	seedFact(t, c, memory.ProjectScope("alpha"), "tabs in alpha")
	hits, _, err := c.Recall(t.Context(), "chat", "codex", "", "tabs", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hitTexts(hits), []string{"tabs in docs", "tabs in makefiles"}; !slices.Equal(got, want) {
		t.Fatalf("recalled %q, want %q", got, want)
	}
}

func TestRecallWithoutScopeSearchesOnlyGlobalWithoutAProject(t *testing.T) {
	c := memoryCoordinator(t)
	useHome(t, c, t.TempDir())
	arrive(c, "chat", memoryOwner, protocol.ChatP2P)
	seedFact(t, c, memory.Global, "tabs in docs")
	seedFact(t, c, memory.ProjectScope("home"), "tabs at home")
	seedFact(t, c, memory.ProjectScope("alpha"), "tabs in alpha")
	hits, _, err := c.Recall(t.Context(), "chat", "codex", "", "tabs", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hitTexts(hits), []string{"tabs in docs"}; !slices.Equal(got, want) {
		t.Fatalf("recalled %q, want %q", got, want)
	}
}

package turn

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// unboundCoordinator is memoryCoordinator with an agent to answer, the
// owner's home project declared, and the owner also owning the feishu
// channel.
func unboundCoordinator(t *testing.T, opts ...testOption) *Coordinator {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	c := memoryCoordinator(t, append([]testOption{withChannelOwner("feishu", memoryOwner), withDeps(func(d *Deps) { d.Catalog = catalog })}, opts...)...)
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "home", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	c.homeProject = "home"
	return c
}

// hear sends a line that answers without binding: the conversation has
// been heard from and is still unbound.
func hear(t *testing.T, c *Coordinator, req Request) {
	t.Helper()
	req.Input = "/status"
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, bound, err := c.projects.Binding(t.Context(), req.ConversationID); err != nil || bound {
		t.Fatalf("/status left %s bound=%v err=%v", req.ConversationID, bound, err)
	}
}

// An unbound conversation reads as the project its first turn will bind:
// home only for the owner in private, the default for a group or a guest
// once one of their lines has arrived, and home for a console
// conversation before any line arrives, since the console only ever
// speaks as the owner.
func TestUnboundConversationReadsAsTheProjectItsFirstTurnBinds(t *testing.T) {
	c := unboundCoordinator(t)
	for _, tc := range []struct {
		name string
		req  Request
		seen bool
		want string
	}{
		{"owner in a group", Request{Channel: "feishu", ConversationID: "oc_group", SenderOpenID: memoryOwner, ChatType: protocol.ChatGroup}, true, "alpha"},
		{"guest in private", Request{Channel: "feishu", ConversationID: "oc_guest", SenderOpenID: "guest", ChatType: protocol.ChatP2P}, true, "alpha"},
		{"owner in private", Request{Channel: "feishu", ConversationID: "oc_owner", SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P}, true, "home"},
		{"new console conversation", Request{Channel: "console", ConversationID: "console:new", SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P}, false, "home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.seen {
				hear(t, c, tc.req)
			}
			read := c.ProjectOf(t.Context(), tc.req.ConversationID)
			context, err := c.Context(t.Context(), tc.req.ConversationID)
			if err != nil || context.Project == nil || context.Project.Bound {
				t.Fatalf("context: %+v err=%v", context.Project, err)
			}
			first, err := c.bindingFor(t.Context(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if first.ProjectID != tc.want || read != first.ProjectID || context.Project.ID != first.ProjectID {
				t.Fatalf("read %q, context %q, first turn bound %q, want %q", read, context.Project.ID, first.ProjectID, tc.want)
			}
		})
	}
}

// A channel conversation this process has not heard from could be a
// group, a guest or the owner in private, and each binds a different
// project. Every read of it says it has no project yet — without an
// error, and without lending it the owner's memory — until a line
// arrives. A restart forgets what was heard.
func TestUnheardChannelConversationReadsAsNoProject(t *testing.T) {
	before := unboundCoordinator(t)
	hear(t, before, Request{Channel: "feishu", ConversationID: "oc_heard", SenderOpenID: memoryOwner, ChatType: protocol.ChatGroup})
	seedFact(t, before, memory.ProjectScope("alpha"), "alpha fact")
	c := unboundCoordinator(t, onLedger(ledgerOf(t, before)), withDeps(func(d *Deps) {
		d.Store, d.Projects, d.Memory = before.store, before.projects, before.memory
	}))
	for _, id := range []string{"oc_heard", "oc_never"} {
		t.Run(id, func(t *testing.T) {
			ctx := t.Context()
			if got := c.ProjectOf(ctx, id); got != "" {
				t.Fatalf("ProjectOf = %q", got)
			}
			context, err := c.Context(ctx, id)
			if err != nil || context.Project != nil {
				t.Fatalf("Context project %+v err=%v", context.Project, err)
			}
			if context.Agent == nil || context.Agent.Place != nil {
				t.Fatalf("Context agent %+v", context.Agent)
			}
			where, err := c.Where(ctx, id, "codex")
			if err != nil || where.Project != "" || where.Workspace != "" || where.Mode != "guest" {
				t.Fatalf("Where %+v err=%v", where, err)
			}
			projects, err := c.WhereProjects(ctx, id, "codex")
			if err != nil || !strings.Contains(projects, "当前项目：（无）") || strings.Contains(projects, "\n* ") {
				t.Fatalf("WhereProjects err=%v\n%s", err, projects)
			}
			if id := c.memoryProject(ctx, id); id != "" {
				t.Fatalf("memoryProject = %q", id)
			}
			if _, scope, err := c.Remember(ctx, id, "codex", "", "project", "", "leak", ""); err == nil {
				t.Fatalf("remembered into %v", scope)
			}
			if hits, _, err := c.Recall(ctx, id, "codex", "", "fact", 10); err == nil {
				t.Fatalf("recalled %+v", hits)
			}
			setup, err := c.SessionSetup(ctx, id, "")
			if err != nil || setup.Mode != "guest" {
				t.Fatalf("SessionSetup mode %q err=%v", setup.Mode, err)
			}
			for _, s := range setup.Sections {
				if strings.HasPrefix(s.Name, "memory:project:") {
					t.Fatalf("SessionSetup carries %+v", s)
				}
			}
		})
	}
}

// The console's setup page shows the contract a console conversation's
// first session opens with, before any line arrives: the owner's, with
// the bound project's memory — the same memory the first turn injects.
func TestConsoleSetupBeforeAnyLineIsTheOwners(t *testing.T) {
	c := unboundCoordinator(t)
	bind(t, c, "console:bound", "beta")
	seedFact(t, c, memory.ProjectScope("beta"), "beta fact")
	for _, tc := range []struct {
		id, memory string
	}{
		{"console:bound", "memory:project:beta"},
		{"console:new", ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			setup, err := c.SessionSetup(t.Context(), tc.id, "")
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, s := range setup.Sections {
				if strings.HasPrefix(s.Name, "memory:project:") {
					got = s.Name
				}
			}
			first := c.projectMemory(t.Context(), tc.id, Request{Channel: "console", ConversationID: tc.id, SenderOpenID: memoryOwner, ChatType: protocol.ChatP2P})
			want := ""
			if len(first) == 1 {
				want = first[0].Name
			}
			if setup.Mode != "owner" || got != tc.memory || got != want {
				t.Fatalf("setup mode %q memory %q, first turn memory %q, want owner and %q", setup.Mode, got, want, tc.memory)
			}
		})
	}
}

package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// An unbound conversation reads as the project its first turn will bind:
// home only for the owner in private, the default for a group or a guest
// once one of their lines has arrived, and home for a console
// conversation before any line arrives, since the console only ever
// speaks as the owner.
func TestUnboundConversationReadsAsTheProjectItsFirstTurnBinds(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	c := memoryCoordinator(t, withChannelOwner("feishu", memoryOwner), withDeps(func(d *Deps) { d.Catalog = catalog }))
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "home", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	c.homeProject = "home"
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
				// A line that answers without binding: the conversation
				// has been heard from and is still unbound.
				status := tc.req
				status.Input = "/status"
				if _, err := c.Handle(t.Context(), status); err != nil {
					t.Fatal(err)
				}
			}
			if _, bound, err := c.projects.Binding(t.Context(), tc.req.ConversationID); err != nil || bound {
				t.Fatalf("precondition: bound=%v err=%v", bound, err)
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

package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

// An unreadable project must stop the turn: its level decides which
// machines may run it, so it cannot be skipped. A missing one is left to
// the checks that already refuse unknown projects.
func TestTurnSpecReportsAnUnreadableProject(t *testing.T) {
	c, _, book, _, req := retainedChatFixture(t)
	c.fleet = roster.New(c.catalog)
	selected, ok := c.catalog.Resolve("worker")
	if !ok {
		t.Fatal("worker is not in the catalog")
	}
	workspace := project.Workspace{ID: "workspace", Project: "p", Node: "node-a", Path: t.TempDir(), Kind: project.KindCanonical}
	if _, _, err := c.turnSpec(t.Context(), req, selected, "task", project.Binding{ProjectID: "missing"}, workspace); err != nil {
		t.Fatalf("missing project: %v", err)
	}
	if err := book.PutBinding(t.Context(), "project", "p", "not a project"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.turnSpec(t.Context(), req, selected, "task", project.Binding{ProjectID: "p"}, workspace); err == nil {
		t.Fatal("an unreadable project was taken as having no level")
	}
}

package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/project"
)

func TestRecoveryContextNamesTheFixedCopyAndRejectsAnotherNode(t *testing.T) {
	c, p, _, workspace := sharedCopy(t)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Node: "node", Default: true}, "elsewhere": {Harness: "mock", Node: "elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	c.catalog = catalog
	if _, err := c.projects.Bind(t.Context(), "console:context", p.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	context, err := c.Context(t.Context(), "console:context")
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range context.Agents {
		if choice.ID == "worker" && (choice.Place == nil || choice.Place.Workspace != workspace.ID || choice.Place.Kind != string(project.KindWorktree) || !choice.Usable) {
			t.Fatalf("context does not describe the actual recovery copy: %+v", choice)
		}
		if choice.ID == "elsewhere" && choice.Usable {
			t.Fatal("context offers another node for a fixed shared recovery copy")
		}
	}
}

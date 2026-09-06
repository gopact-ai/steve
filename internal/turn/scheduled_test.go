package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

func TestScheduledProjectDriftIsRefusedBeforeWorkspaceUse(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "other", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.projects.Bind(t.Context(), "scheduled", "other", "owner"); err != nil {
		t.Fatal(err)
	}
	selected := c.catalog.Default()
	if _, _, err := c.resolveWorkspace(t.Context(), Request{ConversationID: "scheduled", ExpectedProject: "original"}, selected); err == nil {
		t.Fatal("scheduled work followed a changed project binding")
	}
}

package turn

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

// A project whose directory is on another machine used to stop the turn,
// leaving the owner to add a copy by hand before the agent they picked
// could do anything. The turn now asks for one and runs.
func TestTurnGivesTheProjectADirectoryWhereTheAgentRuns(t *testing.T) {
	var attach func(ctx context.Context, projectID, node string) error
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"}, withCallbacks(func(cb *Callbacks) {
		cb.WorkspaceAttach = func(ctx context.Context, projectID, node string) error { return attach(ctx, projectID, node) }
	}))
	dir := filepath.Join(t.TempDir(), "elsewhere")
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "away", Home: project.Home{Node: "machine-a", Path: dir}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.projects.Bind(t.Context(), "thread", "away", "owner"); err != nil {
		t.Fatal(err)
	}
	here := filepath.Join(t.TempDir(), "away")
	asked := 0
	attach = func(ctx context.Context, projectID, node string) error {
		asked++
		if projectID != "away" {
			t.Fatalf("attached the wrong project: %s", projectID)
		}
		return c.projects.Declare(ctx, []project.Project{{ID: "away", Home: project.Home{Node: "machine-a", Path: dir},
			Copies: map[string]project.Copy{node: {Path: here, Origin: project.OriginAdopted, State: project.CopyReady}}}})
	}
	selected := c.catalog.Default()
	_, workspace, err := c.resolveWorkspace(t.Context(), Request{ConversationID: "thread"}, selected)
	if err != nil {
		t.Fatalf("the turn was refused after the project was attached: %v", err)
	}
	if workspace.Path != here || workspace.Kind != project.KindCopy {
		t.Fatalf("the turn ran in %+v, not the directory made for it (%s)", workspace, here)
	}
	if asked != 1 {
		t.Fatalf("the project was asked for %d times, want once", asked)
	}
}

// When attaching leaves the project without a directory on the machine,
// the refusal still says where the project is.
func TestTurnWithoutAttachmentStillSaysWhereTheProjectIs(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"}, withCallbacks(func(cb *Callbacks) {
		cb.WorkspaceAttach = func(context.Context, string, string) error { return nil }
	}))
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "away", Home: project.Home{Node: "machine-a", Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.projects.Bind(t.Context(), "thread", "away", "owner"); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.resolveWorkspace(t.Context(), Request{ConversationID: "thread"}, c.catalog.Default())
	if err == nil || !strings.Contains(err.Error(), "away") {
		t.Fatalf("a project nobody can reach was accepted: %v", err)
	}
}

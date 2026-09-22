package turn

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/project"
)

// A landing stuck because the working tree changed under it has no
// conflicted tree for an agent to work in. The sweeper must leave it for
// the owner instead of failing on it every pass; asking explicitly still
// reports why it cannot be handed over.
func TestAutomaticResolutionSkipsConflictsWithoutATree(t *testing.T) {
	c := New(nil, nil, nil, nil, 0)
	p := project.Project{ID: "p"}
	stuck := []artifact.Stuck{{Artifact: "0992b2aa9a54aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Landing: "land-1"}}
	if got := c.resolveAll(context.Background(), p, stuck, Request{}, true); len(got) != 0 {
		t.Fatalf("sweeper tried a conflict with no tree: %+v", got)
	}
	got := c.resolveAll(context.Background(), p, stuck, Request{}, false)
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("explicit resolution did not explain the missing tree: %+v", got)
	}
}
